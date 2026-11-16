package podtransport

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
)

// Opt-in comparison against original API observations from the isolated fixture.
// Compiler input files are the expected side; observed Pod fields never seed
// source, expiry, profile, image, slot or publication expectations.
func TestRecordedTektonHierarchyMatchesClosedTemplates(t *testing.T) {
	input := os.Getenv("AI_SDLC_BUILD_COMPILED_FIXTURE")
	observed := os.Getenv("AI_SDLC_BUILD_OBSERVED_FIXTURE")
	if input == "" || observed == "" {
		t.Skip("explicit recorded real API fixture required")
	}
	r := &BuildResources{expected: map[string]map[string]any{}}
	read := func(path string) []byte {
		raw, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		return raw
	}
	for _, phase := range []string{"build", "publish"} {
		var obj map[string]any
		if authority.DecodeStrict(read(filepath.Join(input, phase+".json")), &obj) != nil {
			t.Fatal("bad fixture input")
		}
		r.expected[phase] = obj
	}
	build := r.expected["build"]
	pub := r.expected["publish"]
	annotation := build["metadata"].(map[string]any)["annotations"].(map[string]any)
	r.command.ExpiresAt, _ = strconv.ParseInt(annotation["ai-sdlc.expires-at"].(string), 10, 64)
	buildSpec := build["spec"].(map[string]any)
	task := buildSpec["pipelineSpec"].(map[string]any)["tasks"].([]any)[0].(map[string]any)["taskSpec"].(map[string]any)
	guard := task["steps"].([]any)[0].(map[string]any)
	verify := task["steps"].([]any)[1].(map[string]any)
	r.profile = buildworkload.Profile{PublisherImage: guard["image"].(string)}
	for _, raw := range guard["args"].([]any) {
		arg := raw.(string)
		if strings.HasPrefix(arg, "--storage-identity=") {
			r.slot.StorageIdentitySHA256 = strings.TrimPrefix(arg, "--storage-identity=")
		}
		if strings.HasPrefix(arg, "--node-boot-id=") {
			r.slot.NodeBootID = strings.TrimPrefix(arg, "--node-boot-id=")
		}
	}
	r.slot.PVC = task["volumes"].([]any)[0].(map[string]any)["persistentVolumeClaim"].(map[string]any)["claimName"].(string)
	r.slot.Node = buildSpec["taskRunTemplate"].(map[string]any)["podTemplate"].(map[string]any)["nodeSelector"].(map[string]any)["kubernetes.io/hostname"].(string)
	for _, raw := range verify["env"].([]any) {
		env := raw.(map[string]any)
		if env["name"] == "AI_SDLC_SOURCE_SHA" {
			r.command.SourceSHA = env["value"].(string)
		}
		if env["name"] == "AI_SDLC_SOURCE_TREE" {
			r.command.TreeSHA = env["value"].(string)
		}
	}
	r.compiled.BuildScriptBase64 = base64.StdEncoding.EncodeToString([]byte(verify["script"].(string)))
	args := pub["spec"].(map[string]any)["pipelineSpec"].(map[string]any)["tasks"].([]any)[0].(map[string]any)["taskSpec"].(map[string]any)["steps"].([]any)[1].(map[string]any)["args"].([]any)
	r.compiled.Plan = building.Plan{}
	for _, raw := range args {
		arg := raw.(string)
		if strings.HasPrefix(arg, "--output=") {
			r.compiled.Plan.Output = strings.TrimPrefix(arg, "--output=")
		}
		if strings.HasPrefix(arg, "--definition-digest=sha256:") {
			r.compiled.Plan.DefinitionSHA256 = strings.TrimPrefix(arg, "--definition-digest=sha256:")
		}
	}
	phases := []string{"build", "publish"}
	if os.Getenv("AI_SDLC_BUILD_OBSERVED_PHASE") == "build" {
		phases = []string{"build"}
	}
	for _, phase := range phases {
		runRaw := read(filepath.Join(observed, phase+"-run.json"))
		runUID, _, e := r.checkRun(runRaw, phase, "")
		if e != nil {
			t.Fatalf("%s actual PipelineRun defaulting differs: %v", phase, e)
		}
		var recordedRun map[string]any
		_ = json.Unmarshal(runRaw, &recordedRun)
		if status, ok := recordedRun["status"].(map[string]any); ok {
			if resolved, exists := status["pipelineSpec"]; exists {
				wanted := defaultRunSpec(r.expected[phase]["spec"].(map[string]any))["pipelineSpec"].(map[string]any)
				if phase == "publish" {
					wanted["tasks"].([]any)[0].(map[string]any)["params"].([]any)[0].(map[string]any)["value"] = runUID
				}
				if !reflect.DeepEqual(resolved, wanted) {
					t.Fatal("resolved Pipeline differs", changedBuildPaths(resolved, wanted, "pipelineSpec"))
				}
			}
		}
		taskRaw := read(filepath.Join(observed, phase+"-task.json"))
		taskUID, _, e := r.checkTask(taskRaw, phase, runUID)
		if e != nil {
			t.Fatalf("%s actual TaskRun differs: %v", phase, e)
		}
		var recordedTask map[string]any
		_ = json.Unmarshal(taskRaw, &recordedTask)
		if status, ok := recordedTask["status"].(map[string]any); ok {
			if resolved, exists := status["taskSpec"]; exists && !reflect.DeepEqual(resolved, r.expectedResolvedTask(phase, runUID)) {
				t.Fatal("resolved task differs", changedBuildPaths(resolved, r.expectedResolvedTask(phase, runUID), "taskSpec"))
			}
		}
		podRaw := read(filepath.Join(observed, phase+"-pod.json"))
		if _, _, _, e := r.checkOwnedBuildPod(podRaw, phase, runUID, taskUID); e != nil {
			var actual map[string]any
			_ = json.Unmarshal(podRaw, &actual)
			expected, _ := r.expectedBuildPodSpec(phase, runUID, actual["spec"].(map[string]any))
			t.Log("differing spec paths", changedBuildPaths(actual["spec"], expected, "spec"))
			t.Fatalf("%s actual Pod differs: %v", phase, e)
		}
		var recordedPod map[string]any
		_ = json.Unmarshal(podRaw, &recordedPod)
		if status, ok := recordedPod["status"].(map[string]any); ok {
			if _, _, err := inspectContainerStates(recordedPod["spec"].(map[string]any), status, &buildPins{}); err != nil {
				t.Fatal("recorded container states rejected", err)
			}
		}
		for _, which := range []string{"image", "command", "environment", "mount", "secret", "security", "deadline", "unknown", "source", "run-uid", "alias", "script"} {
			if phase == "build" && (which == "run-uid" || which == "alias") {
				continue
			}
			if phase == "publish" && which == "script" {
				continue
			}
			var pod map[string]any
			_ = json.Unmarshal(podRaw, &pod)
			spec := pod["spec"].(map[string]any)
			step := spec["containers"].([]any)[1].(map[string]any)
			switch which {
			case "image":
				step["image"] = "untrusted.invalid/image:latest"
			case "command":
				step["command"] = []any{"/bin/sh"}
			case "environment":
				step["env"] = append(step["env"].([]any), map[string]any{"name": "INJECT", "value": "x"})
			case "mount":
				step["volumeMounts"] = append(step["volumeMounts"].([]any), map[string]any{"name": "tekton-internal-home", "mountPath": "/slot/workspace"})
			case "secret":
				spec["volumes"] = append(spec["volumes"].([]any), map[string]any{"name": "foreign", "secret": map[string]any{"secretName": "foreign"}})
			case "security":
				step["securityContext"].(map[string]any)["allowPrivilegeEscalation"] = true
			case "deadline":
				spec["activeDeadlineSeconds"] = float64(9999)
			case "unknown":
				spec["unreviewedFutureField"] = true
			case "source":
				if phase == "build" {
					step["env"].([]any)[0].(map[string]any)["value"] = strings.Repeat("0", 40)
				} else {
					step["args"].([]any)[19] = "--source-sha=" + strings.Repeat("0", 40)
				}
			case "run-uid":
				step["args"].([]any)[21] = "--run-uid=" + fixtureOtherUID
			case "alias":
				for _, raw := range step["volumeMounts"].([]any) {
					m := raw.(map[string]any)
					if credsAlias.MatchString(m["name"].(string)) {
						m["name"] = "tekton-creds-00000000"
					}
				}
			case "script":
				spec["initContainers"].([]any)[1].(map[string]any)["args"].([]any)[1] = "echo injected"
			}
			changed, _ := json.Marshal(pod)
			if _, e := r.checkBuildPodSpec(changed, phase, runUID); e == nil {
				t.Fatalf("%s/%s mutation accepted", phase, which)
			}
		}
	}
}

// Only field paths are emitted, never credential or observed field values.
func changedBuildPaths(a, b any, path string) []string {
	if reflect.DeepEqual(a, b) {
		return nil
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		out := []string{}
		for k := range keys {
			out = append(out, changedBuildPaths(am[k], bm[k], path+"."+k)...)
		}
		sort.Strings(out)
		return out
	}
	aa, aok := a.([]any)
	bb, bok := b.([]any)
	if aok && bok && len(aa) == len(bb) {
		out := []string{}
		for i := range aa {
			out = append(out, changedBuildPaths(aa[i], bb[i], path+"["+strconv.Itoa(i)+"]")...)
		}
		return out
	}
	return []string{path}
}

func TestBuildShellAliasQuoting(t *testing.T) {
	for _, value := range []string{`/tekton/scripts/script-1-abc12`, `x";touch /tmp/unwanted;"`, "$(id)`id`\\", "line\nend"} {
		quoted := quoteBuildShellValue(value)
		command := exec.Command("sh", "-c", "printf '%s' \""+quoted+"\"")
		got, err := command.Output()
		if err != nil || string(got) != value {
			t.Fatalf("quoted round trip failed: %v", err)
		}
	}
}
