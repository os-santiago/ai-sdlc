package podtransport

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
)

const tektonRelease = "70f7308"

func (r *BuildResources) taskName(phase string) string { return r.runName(phase) + "-" + phase }
func (r *BuildResources) checkTask(raw []byte, phase, runUID string) (string, map[string]any, error) {
	if phase != "build" && phase != "publish" || !resourceUID.MatchString(runUID) {
		return "", nil, ErrBuildTransport
	}
	var obj map[string]any
	if len(raw) > maxBuildObject || authority.DecodeStrict(raw, &obj) != nil || obj["apiVersion"] != "tekton.dev/v1" || obj["kind"] != "TaskRun" {
		return "", nil, ErrBuildTransport
	}
	metadata, ok := obj["metadata"].(map[string]any)
	if !ok {
		return "", nil, ErrBuildTransport
	}
	uid, e := r.checkChildMetadata(metadata, phase, runUID, "", false)
	if e != nil {
		return "", nil, e
	}
	runSpec := defaultRunSpec(r.expected[phase]["spec"].(map[string]any))
	task := cloneObject(runSpec["pipelineSpec"].(map[string]any)["tasks"].([]any)[0].(map[string]any)["taskSpec"].(map[string]any))
	delete(task, "metadata")
	delete(task, "spec")
	template := runSpec["taskRunTemplate"].(map[string]any)
	expected := map[string]any{"taskSpec": task, "podTemplate": template["podTemplate"], "serviceAccountName": phase, "timeout": runSpec["timeouts"].(map[string]any)["pipeline"]}
	if phase == "publish" {
		expected["params"] = []any{map[string]any{"name": "run-uid", "value": runUID}}
	}
	if !reflect.DeepEqual(obj["spec"], expected) {
		return "", nil, ErrBuildTransport
	}
	return uid, obj, nil
}
func (r *BuildResources) checkChildMetadata(metadata map[string]any, phase, runUID, taskUID string, pod bool) (string, error) {
	if phase != "build" && phase != "publish" || !resourceUID.MatchString(runUID) || (pod && !resourceUID.MatchString(taskUID)) {
		return "", ErrBuildTransport
	}
	name := r.taskName(phase)
	parentName := r.runName(phase)
	parentKind := "PipelineRun"
	parentUID := runUID
	if pod {
		name += "-pod"
		parentName = r.taskName(phase)
		parentKind = "TaskRun"
		parentUID = taskUID
	}
	uid, ok := metadata["uid"].(string)
	if !ok || !resourceUID.MatchString(uid) || uid == runUID || uid == taskUID || metadata["name"] != name || metadata["namespace"] != buildworkload.Namespace || metadata["generateName"] != nil || metadata["deletionTimestamp"] != nil {
		return "", ErrBuildTransport
	}
	owner := []any{map[string]any{"apiVersion": "tekton.dev/v1", "kind": parentKind, "name": parentName, "uid": parentUID, "controller": true, "blockOwnerDeletion": true}}
	if !reflect.DeepEqual(metadata["ownerReferences"], owner) {
		return "", ErrBuildTransport
	}
	original := r.expected[phase]["metadata"].(map[string]any)
	labels := cloneObject(original["labels"].(map[string]any))
	for k, v := range map[string]any{"app.kubernetes.io/managed-by": "tekton-pipelines", "tekton.dev/memberOf": "tasks", "tekton.dev/pipeline": r.runName(phase), "tekton.dev/pipelineRun": r.runName(phase), "tekton.dev/pipelineRunUID": runUID, "tekton.dev/pipelineTask": phase} {
		labels[k] = v
	}
	if pod {
		labels["tekton.dev/taskRun"] = r.taskName(phase)
		labels["tekton.dev/taskRunUID"] = taskUID
	}
	if !reflect.DeepEqual(metadata["labels"], labels) {
		return "", ErrBuildTransport
	}
	annotations, ok := metadata["annotations"].(map[string]any)
	if !ok {
		return "", ErrBuildTransport
	}
	required := original["annotations"].(map[string]any)
	for key, value := range required {
		if annotations[key] != value {
			return "", ErrBuildTransport
		}
	}
	for key, value := range annotations {
		if _, ok := required[key]; ok {
			continue
		}
		text, ok := value.(string)
		if !ok || len(text) > 2048 {
			return "", ErrBuildTransport
		}
		switch key {
		case "pipeline.tekton.dev/release":
			if text != tektonRelease {
				return "", ErrBuildTransport
			}
		case "tekton.dev/ready":
			if !pod || text != "READY" {
				return "", ErrBuildTransport
			}
		case "kubernetes.io/limit-ranger":
			expected := "LimitRanger plugin set: cpu, ephemeral-storage, memory request for init container prepare; cpu, ephemeral-storage, memory limit for init container prepare"
			if phase == "build" {
				expected += "; cpu, ephemeral-storage, memory request for init container place-scripts; cpu, ephemeral-storage, memory limit for init container place-scripts"
			}
			if !pod || text != expected {
				return "", ErrBuildTransport
			}
		case "results.tekton.dev/record", "results.tekton.dev/result", "results.tekton.dev/stored", "chains.tekton.dev/signed":
			if pod {
				return "", ErrBuildTransport
			}
		default:
			return "", ErrBuildTransport
		}
	}
	if annotations["pipeline.tekton.dev/release"] != tektonRelease {
		return "", ErrBuildTransport
	}
	if value, exists := metadata["finalizers"]; exists {
		finals, ok := value.([]any)
		if !ok || pod && len(finals) > 0 {
			return "", ErrBuildTransport
		}
		seen := map[string]bool{}
		for _, value := range finals {
			text, ok := value.(string)
			if !ok || seen[text] || (text != "chains.tekton.dev/taskrun" && text != "results.tekton.dev/taskrun") {
				return "", ErrBuildTransport
			}
			seen[text] = true
		}
	}
	return uid, nil
}
func (r *BuildResources) checkOwnedBuildPod(raw []byte, phase, runUID, taskUID string) (string, string, map[string]any, error) {
	digest, e := r.checkBuildPodSpec(raw, phase, runUID)
	if e != nil {
		return "", "", nil, e
	}
	var pod map[string]any
	if authority.DecodeStrict(raw, &pod) != nil {
		return "", "", nil, ErrBuildTransport
	}
	metadata, ok := pod["metadata"].(map[string]any)
	if !ok {
		return "", "", nil, ErrBuildTransport
	}
	uid, e := r.checkChildMetadata(metadata, phase, runUID, taskUID, true)
	if e != nil {
		return "", "", nil, e
	}
	return uid, digest, pod, nil
}

// Task status.spec is resolved by Tekton; compare its exact compiler-derived
// content, resolving only the finite publish run-uid parameter.
func (r *BuildResources) expectedResolvedTask(phase, runUID string) map[string]any {
	spec := defaultRunSpec(r.expected[phase]["spec"].(map[string]any))["pipelineSpec"].(map[string]any)["tasks"].([]any)[0].(map[string]any)["taskSpec"].(map[string]any)
	delete(spec, "metadata")
	delete(spec, "spec")
	if phase == "publish" {
		steps := spec["steps"].([]any)
		for _, raw := range steps {
			step := raw.(map[string]any)
			if args, ok := step["args"].([]any); ok {
				for i, value := range args {
					if text, ok := value.(string); ok && strings.Contains(text, "$(params.run-uid)") {
						args[i] = strings.ReplaceAll(text, "$(params.run-uid)", runUID)
					}
				}
			}
		}
	}
	// Decode through JSON for an independent copy and stable scalar types.
	raw, _ := json.Marshal(spec)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}
