package podtransport

import (
	"embed"
	"encoding/json"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
)

// Templates capture reviewed Kubernetes1.36.4/Tekton1.15 defaulting and the
// mandatory bounds policy. Version/config drift fails closed until re-reviewed.
//
//go:embed build_templates/*.json
var buildPodTemplates embed.FS
var scriptBasename = regexp.MustCompile(`^/tekton/scripts/script-1-[a-z0-9]{5}$`)
var credsAlias = regexp.MustCompile(`^tekton-creds-[a-f0-9]{8}$`)

func (r *BuildResources) expectedBuildPodSpec(phase, runUID string, actual map[string]any) (map[string]any, error) {
	if phase != "build" && phase != "publish" || !resourceUID.MatchString(runUID) {
		return nil, ErrBuildTransport
	}
	raw, e := buildPodTemplates.ReadFile("build_templates/" + phase + "-v1.json")
	if e != nil {
		return nil, ErrBuildTransport
	}
	var expected map[string]any
	if authority.DecodeStrict(raw, &expected) != nil {
		return nil, ErrBuildTransport
	}
	// Extract only unavoidable syntactic aliases. No observed execution value is
	// copied into trusted expectations. Alias consistency follows full equality.
	scriptFile := ""
	alias := ""
	if phase == "build" {
		containers, ok := actual["containers"].([]any)
		if !ok || len(containers) != 2 {
			return nil, ErrBuildTransport
		}
		verify, ok := containers[1].(map[string]any)
		if !ok || verify["name"] != "step-verify" {
			return nil, ErrBuildTransport
		}
		args, ok := verify["args"].([]any)
		if !ok || len(args) != 13 {
			return nil, ErrBuildTransport
		}
		scriptFile, ok = args[11].(string)
		if !ok || !scriptBasename.MatchString(scriptFile) {
			return nil, ErrBuildTransport
		}
	} else {
		volumes, ok := actual["volumes"].([]any)
		if !ok {
			return nil, ErrBuildTransport
		}
		for _, raw := range volumes {
			v, ok := raw.(map[string]any)
			if !ok {
				return nil, ErrBuildTransport
			}
			name, ok := v["name"].(string)
			if !ok {
				return nil, ErrBuildTransport
			}
			if credsAlias.MatchString(name) {
				if alias != "" {
					return nil, ErrBuildTransport
				}
				alias = name
			}
		}
		if alias == "" {
			return nil, ErrBuildTransport
		}
	}
	expected["nodeSelector"] = map[string]any{"kubernetes.io/hostname": r.slot.Node}
	if node, exists := actual["nodeName"]; exists {
		if node != r.slot.Node {
			return nil, ErrBuildTransport
		}
		expected["nodeName"] = r.slot.Node
	}
	for _, raw := range expected["volumes"].([]any) {
		v := raw.(map[string]any)
		if v["name"] == "slot" {
			v["persistentVolumeClaim"].(map[string]any)["claimName"] = r.slot.PVC
		}
		if credsAlias.MatchString(v["name"].(string)) {
			v["name"] = alias
		}
	}
	for _, raw := range expected["containers"].([]any) {
		c := raw.(map[string]any)
		name := c["name"].(string)
		if name != "step-verify" {
			c["image"] = r.profile.PublisherImage
		} else {
			c["image"] = buildworkload.GoImage
		}
		args := c["args"].([]any)
		for i, value := range args {
			text := value.(string)
			switch {
			case strings.HasPrefix(text, "--storage-identity="):
				args[i] = "--storage-identity=" + r.slot.StorageIdentitySHA256
			case strings.HasPrefix(text, "--node-boot-id="):
				args[i] = "--node-boot-id=" + r.slot.NodeBootID
			case strings.HasPrefix(text, "--authority-expires-at="):
				args[i] = "--authority-expires-at=" + strconv.FormatInt(r.command.ExpiresAt, 10)
			case strings.HasPrefix(text, "--output="):
				args[i] = "--output=" + r.compiled.Plan.Output
			case strings.HasPrefix(text, "--source-sha="):
				args[i] = "--source-sha=" + r.command.SourceSHA
			case strings.HasPrefix(text, "--definition-digest="):
				args[i] = "--definition-digest=sha256:" + r.compiled.Plan.DefinitionSHA256
			case strings.HasPrefix(text, "--run-uid="):
				args[i] = "--run-uid=" + runUID
			case scriptBasename.MatchString(text):
				args[i] = scriptFile
			}
		}
		for _, raw := range c["env"].([]any) {
			env := raw.(map[string]any)
			if env["name"] == "AI_SDLC_SOURCE_SHA" {
				env["value"] = r.command.SourceSHA
			}
			if env["name"] == "AI_SDLC_SOURCE_TREE" {
				env["value"] = r.command.TreeSHA
			}
		}
		for _, raw := range c["volumeMounts"].([]any) {
			m := raw.(map[string]any)
			if credsAlias.MatchString(m["name"].(string)) {
				m["name"] = alias
			}
		}
	}
	if phase == "build" {
		initializer := expected["initContainers"].([]any)[1].(map[string]any)
		initializer["args"] = []any{"-c", "scriptfile=\"" + scriptFile + "\"\ntouch ${scriptfile} && chmod +x ${scriptfile}\ncat > ${scriptfile} << '_EOF_'\n" + r.compiled.BuildScriptBase64 + "\n_EOF_\n/tekton/bin/entrypoint decode-script \"${scriptfile}\"\n"}
	}
	return expected, nil
}
func (r *BuildResources) checkBuildPodSpec(raw []byte, phase, runUID string) (string, error) {
	var pod map[string]any
	if len(raw) > maxBuildObject || authority.DecodeStrict(raw, &pod) != nil || pod["apiVersion"] != "v1" || pod["kind"] != "Pod" {
		return "", ErrBuildTransport
	}
	actual, ok := pod["spec"].(map[string]any)
	if !ok {
		return "", ErrBuildTransport
	}
	expected, e := r.expectedBuildPodSpec(phase, runUID, actual)
	if e != nil || !reflect.DeepEqual(actual, expected) {
		return "", ErrBuildTransport
	}
	canonical, _ := json.Marshal(actual)
	return authority.Digest(canonical), nil
}
