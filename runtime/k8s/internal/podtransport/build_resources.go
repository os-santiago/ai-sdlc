package podtransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
)

var ErrBuildTransport = errors.New("closed build transport denied")
var ErrUnknownBuildCreate = errors.New("build create outcome unknown; original intent must be quarantined")

const maxBuildObject = 1024 * 1024

type BuildResources struct {
	poisoned  bool
	completed map[string]building.Termination
	mu        sync.Mutex
	attempted map[string]bool
	knownRuns map[string]string
	phasePins map[string]*buildPins
	client    *Client
	compiled  buildworkload.Compiled
	command   building.Command
	slot      building.Slot
	slots     []building.Slot
	profile   buildworkload.Profile
	expected  map[string]map[string]any
}

// PlanBuild compiles only authenticated finite inputs. It performs no API write.
// The owning controller must persist its Plan and fresh create intent before POST.
func PlanBuild(client *Client, cfg building.Configuration, p buildworkload.Profile, signed authority.Receipt, payload []byte, slot building.Slot) (*BuildResources, error) {
	if client == nil || client.http == nil || client.config == nil {
		return nil, ErrBuildTransport
	}
	w, e := buildworkload.Compile(cfg, p, signed, payload, slot, time.Now())
	if e != nil {
		return nil, ErrBuildTransport
	}
	var command building.Command
	if authority.DecodeStrict(payload, &command) != nil {
		return nil, ErrBuildTransport
	}
	profileBytes, _ := json.Marshal(p)
	var privateProfile buildworkload.Profile
	if authority.DecodeStrict(profileBytes, &privateProfile) != nil {
		return nil, ErrBuildTransport
	}
	r := &BuildResources{completed: map[string]building.Termination{}, attempted: map[string]bool{}, knownRuns: map[string]string{}, phasePins: map[string]*buildPins{}, client: client, compiled: w, command: command, slot: slot, slots: append([]building.Slot(nil), cfg.Slots...), profile: privateProfile, expected: map[string]map[string]any{}}
	for phase, raw := range map[string][]byte{"build": w.Build, "publish": w.Publish} {
		var obj map[string]any
		if authority.DecodeStrict(raw, &obj) != nil {
			return nil, ErrBuildTransport
		}
		r.expected[phase] = obj
	}
	return r, nil
}
func (r *BuildResources) Plan() building.Plan { return r.compiled.Plan }
func (r *BuildResources) request(ctx context.Context, method, path string, input []byte, cleanup bool) ([]byte, int, error) {
	if r == nil || r.client == nil || len(input) > maxBuildObject || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#\\\r\n") {
		return nil, 0, ErrBuildTransport
	}
	deadline := time.Now().Add(5 * time.Second)
	if !cleanup {
		if !time.Now().Before(r.compiled.ExpiresAt) {
			return nil, 0, ErrBuildTransport
		}
		deadline = minDeadline(deadline, r.compiled.ExpiresAt)
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, method, r.client.base+path, bytes.NewReader(input))
	if e != nil {
		return nil, 0, ErrBuildTransport
	}
	req.Header.Set("Accept", "application/json")
	if len(input) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.client.config.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.client.config.BearerToken)
	}
	response, e := r.client.http.Do(req)
	if e != nil {
		return nil, 0, ErrBuildTransport
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, maxBuildObject+1))
	if e != nil || len(raw) > maxBuildObject || ctx.Err() != nil || (!cleanup && !time.Now().Before(r.compiled.ExpiresAt)) {
		return nil, 0, ErrBuildTransport
	}
	if response.StatusCode == http.StatusForbidden {
		return nil, response.StatusCode, ErrPermission
	}
	return raw, response.StatusCode, nil
}
func (r *BuildResources) runName(phase string) string {
	if x, ok := r.expected[phase]; ok {
		return x["metadata"].(map[string]any)["name"].(string)
	}
	return ""
}
func runPath(name string) string {
	path := "/apis/tekton.dev/v1/namespaces/" + buildworkload.Namespace + "/pipelineruns"
	if name != "" {
		path += "/" + name
	}
	return path
}
func cloneObject(v map[string]any) map[string]any {
	raw, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// Expected Tekton 1.15 defaults are applied only to the trusted template. Unknown
// fields in a response remain visible and fail equality; nothing is stripped.
func defaultRunSpec(expected map[string]any) map[string]any {
	spec := cloneObject(expected)
	timeout := spec["timeouts"].(map[string]any)
	timeout["pipeline"] = canonicalDuration(timeout["pipeline"].(string))
	pipeline := spec["pipelineSpec"].(map[string]any)
	for _, raw := range pipeline["tasks"].([]any) {
		task := raw.(map[string]any)
		task["timeout"] = canonicalDuration(task["timeout"].(string))
		ts := task["taskSpec"].(map[string]any)
		ts["metadata"] = map[string]any{}
		ts["spec"] = nil
		for _, raw := range ts["steps"].([]any) {
			step := raw.(map[string]any)
			step["timeout"] = canonicalDuration(step["timeout"].(string))
			for _, raw := range step["volumeMounts"].([]any) {
				m := raw.(map[string]any)
				if m["readOnly"] == false {
					delete(m, "readOnly")
				}
			}
		}
		for _, raw := range ts["volumes"].([]any) {
			v := raw.(map[string]any)
			if pvc, ok := v["persistentVolumeClaim"].(map[string]any); ok && pvc["readOnly"] == false {
				delete(pvc, "readOnly")
			}
		}
	}
	if results, ok := pipeline["results"].([]any); ok {
		for _, raw := range results {
			raw.(map[string]any)["description"] = ""
		}
	}
	return spec
}
func canonicalDuration(value string) string {
	d, e := time.ParseDuration(value)
	if e != nil {
		return "INVALID"
	}
	return d.String()
}
func (r *BuildResources) checkRun(raw []byte, phase, uid string) (string, map[string]any, error) {
	var obj map[string]any
	if len(raw) > maxBuildObject || authority.DecodeStrict(raw, &obj) != nil || obj["apiVersion"] != "tekton.dev/v1" || obj["kind"] != "PipelineRun" {
		return "", nil, ErrBuildTransport
	}
	expected, ok := r.expected[phase]
	if !ok {
		return "", nil, ErrBuildTransport
	}
	metadata, ok := obj["metadata"].(map[string]any)
	if !ok {
		return "", nil, ErrBuildTransport
	}
	wanted := expected["metadata"].(map[string]any)
	actualUID, ok := metadata["uid"].(string)
	if !ok || !resourceUID.MatchString(actualUID) || (uid != "" && actualUID != uid) || metadata["name"] != wanted["name"] || metadata["namespace"] != buildworkload.Namespace || metadata["generateName"] != nil || metadata["deletionTimestamp"] != nil {
		return "", nil, ErrBuildTransport
	}
	for _, field := range []string{"labels", "annotations"} {
		got, ok := metadata[field].(map[string]any)
		if !ok {
			return "", nil, ErrBuildTransport
		}
		want := wanted[field].(map[string]any)
		for k, v := range want {
			if got[k] != v {
				return "", nil, ErrBuildTransport
			}
		}
		for k, v := range got {
			if _, exists := want[k]; exists {
				continue
			}
			if field == "labels" && k == "tekton.dev/pipeline" && v == wanted["name"] {
				continue
			}
			if field == "annotations" && (k == "results.tekton.dev/record" || k == "results.tekton.dev/result" || k == "results.tekton.dev/stored" || k == "chains.tekton.dev/signed") {
				if text, ok := v.(string); ok && len(text) <= 2048 {
					continue
				}
			}
			return "", nil, ErrBuildTransport
		}
	}
	if owners, ok := metadata["ownerReferences"].([]any); ok && len(owners) > 0 {
		return "", nil, ErrBuildTransport
	}
	if finals, ok := metadata["finalizers"].([]any); ok {
		for _, v := range finals {
			if v != "results.tekton.dev/pipelinerun" && v != "chains.tekton.dev/pipelinerun" {
				return "", nil, ErrBuildTransport
			}
		}
	}
	spec, ok := obj["spec"].(map[string]any)
	if !ok || !reflect.DeepEqual(spec, defaultRunSpec(expected["spec"].(map[string]any))) {
		return "", nil, ErrBuildTransport
	}
	return actualUID, obj, nil
}

// postRun is deliberately private until installation preflight and journal-backed
// controller integration are complete. It never retries or adopts a named run.
func (r *BuildResources) postRun(ctx context.Context, phase string) (string, error) {
	if r == nil {
		return "", ErrBuildTransport
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.poisoned || r.compiled.MayCreate(phase, time.Now()) != nil || r.attempted[phase] {
		return "", ErrBuildTransport
	}
	if phase == "publish" {
		original, ok := r.completed["build"]
		if !ok || original.PipelineRunUID != r.knownRuns["build"] || original.Validate(r.command, r.compiled.Plan, r.slot, time.Now()) != nil {
			return "", ErrBuildTransport
		}
	}
	raw := r.compiled.Build
	if phase == "publish" {
		raw = r.compiled.Publish
	}
	r.attempted[phase] = true
	response, status, e := r.request(ctx, http.MethodPost, runPath(""), raw, false)
	if e != nil || status != http.StatusCreated {
		return "", ErrUnknownBuildCreate
	}
	uid, _, e := r.checkRun(response, phase, "")
	if e != nil {
		return "", ErrUnknownBuildCreate
	}
	r.knownRuns[phase] = uid
	return uid, nil
}
