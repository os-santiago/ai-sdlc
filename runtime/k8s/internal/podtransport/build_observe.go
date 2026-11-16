package podtransport

import (
	"context"
	"reflect"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
)

type BuildObservation struct {
	Complete    bool
	Termination building.Termination
	Results     map[string]string
}

func objectOwnerUID(obj map[string]any, kind, uid string) bool {
	metadata, ok := obj["metadata"].(map[string]any)
	if !ok {
		return false
	}
	owners, ok := metadata["ownerReferences"].([]any)
	if !ok {
		return false
	}
	for _, raw := range owners {
		owner, ok := raw.(map[string]any)
		if ok && owner["kind"] == kind && owner["uid"] == uid {
			return true
		}
	}
	return false
}
func buildLabel(obj map[string]any, key string) string {
	m, ok := obj["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	labels, ok := m["labels"].(map[string]any)
	if !ok {
		return ""
	}
	text, _ := labels[key].(string)
	return text
}
func (r *BuildResources) listBuildObjects(ctx context.Context, path string) ([]map[string]any, error) {
	obj, _, e := r.getBuildObject(ctx, path)
	if e != nil {
		return nil, e
	}
	metadata, ok := obj["metadata"].(map[string]any)
	if !ok {
		return nil, ErrBuildTransport
	}
	if next, exists := metadata["continue"]; exists && next != "" {
		return nil, ErrBuildTransport
	}
	items, ok := obj["items"].([]any)
	if !ok || len(items) > 128 {
		return nil, ErrBuildTransport
	}
	result := []map[string]any{}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, ErrBuildTransport
		}
		result = append(result, item)
	}
	return result, nil
}

// observePhase is private until the controller supplies its original acknowledged
// Run UID and retains pins throughout a single claim. Restarted claims fail into
// cleanup; they never reconstruct pins and continue publishing.
func (r *BuildResources) observePhase(ctx context.Context, phase, runUID string) (out BuildObservation, err error) {
	if r == nil {
		return out, ErrBuildTransport
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	defer func() {
		if _, wasComplete := r.completed[phase]; wasComplete && !out.Complete && err == nil {
			err = ErrBuildTransport
		}
		if err != nil {
			r.poisoned = true
			delete(r.completed, phase)
		}
	}()
	if r.poisoned {
		return out, ErrBuildTransport
	}
	if r.knownRuns[phase] != runUID || (phase != "build" && phase != "publish") || !resourceUID.MatchString(runUID) {
		return out, ErrBuildTransport
	}
	pins := r.phasePins[phase]
	if pins == nil {
		pins = &buildPins{}
		r.phasePins[phase] = pins
	}
	raw, status, e := r.request(ctx, "GET", runPath(r.runName(phase)), nil, false)
	if e != nil || status != 200 {
		return out, ErrBuildTransport
	}
	_, run, e := r.checkRun(raw, phase, runUID)
	if e != nil {
		return out, e
	}
	runDone, e := succeededStatus(run)
	if e != nil {
		return out, e
	}
	tasks, e := r.listBuildObjects(ctx, "/apis/tekton.dev/v1/namespaces/"+buildworkload.Namespace+"/taskruns")
	if e != nil {
		return out, e
	}
	pods, e := r.listBuildObjects(ctx, "/api/v1/namespaces/"+buildworkload.Namespace+"/pods")
	if e != nil {
		return out, e
	}
	if e := r.checkSlotOccupants(pods, phase, runUID); e != nil {
		return out, e
	}
	children := []map[string]any{}
	for _, task := range tasks {
		if objectOwnerUID(task, "PipelineRun", runUID) || buildLabel(task, "tekton.dev/pipelineRunUID") == runUID || buildLabel(task, "tekton.dev/pipelineRun") == r.runName(phase) {
			children = append(children, task)
		}
	}
	if len(children) > 1 {
		return out, ErrBuildTransport
	}
	var task map[string]any
	taskUID := ""
	if len(children) == 1 {
		task = children[0]
		taskUID, _, e = r.checkTask(marshalObject(task), phase, runUID)
		if e != nil {
			return out, e
		}
		if pins.TaskUID != "" && pins.TaskUID != taskUID {
			return out, ErrBuildTransport
		}
		pins.TaskUID = taskUID
	} else if pins.TaskUID != "" || runDone {
		return out, ErrBuildTransport
	}
	writers := []map[string]any{}
	for _, pod := range pods {
		if (taskUID != "" && objectOwnerUID(pod, "TaskRun", taskUID)) || buildLabel(pod, "tekton.dev/pipelineRunUID") == runUID || buildLabel(pod, "tekton.dev/pipelineRun") == r.runName(phase) {
			writers = append(writers, pod)
		}
	}
	if len(writers) > 1 || task == nil && len(writers) > 0 {
		return out, ErrBuildTransport
	}
	if task == nil {
		return out, nil
	}
	taskDone, e := succeededStatus(task)
	if e != nil {
		return out, e
	}
	taskStatus, _ := task["status"].(map[string]any)
	if taskStatus != nil {
		if resolved, exists := taskStatus["taskSpec"]; exists && !reflect.DeepEqual(resolved, r.expectedResolvedTask(phase, runUID)) {
			return out, ErrBuildTransport
		}
	}
	if len(writers) == 0 {
		if pins.PodUID != "" || taskDone || runDone {
			return out, ErrBuildTransport
		}
		return out, nil
	}
	writer := writers[0]
	podUID, specDigest, _, e := r.checkOwnedBuildPod(marshalObject(writer), phase, runUID, taskUID)
	if e != nil {
		return out, e
	}
	if pins.PodUID != "" && pins.PodUID != podUID {
		return out, ErrBuildTransport
	}
	pins.PodUID = podUID
	podSpec := writer["spec"].(map[string]any)
	podStatus, _ := writer["status"].(map[string]any)
	if podStatus == nil {
		if len(pins.Containers) > 0 || runDone || taskDone {
			return out, ErrBuildTransport
		}
		return out, nil
	}
	if podStatus["phase"] == "Failed" {
		return out, ErrBuildTransport
	}
	exits, complete, e := inspectContainerStates(podSpec, podStatus, pins)
	if e != nil {
		return out, e
	}
	if !complete || !runDone || !taskDone {
		return out, nil
	}
	if podStatus["phase"] != "Succeeded" || podSpec["nodeName"] != r.slot.Node || taskStatus["podName"] != r.taskName(phase)+"-pod" || !reflect.DeepEqual(taskStatus["taskSpec"], r.expectedResolvedTask(phase, runUID)) {
		return out, ErrBuildTransport
	}
	runStatus, ok := run["status"].(map[string]any)
	if !ok {
		return out, ErrBuildTransport
	}
	refs := []any{map[string]any{"apiVersion": "tekton.dev/v1", "kind": "TaskRun", "name": r.taskName(phase), "pipelineTaskName": phase}}
	if !reflect.DeepEqual(runStatus["childReferences"], refs) {
		return out, ErrBuildTransport
	}
	expectedRun := defaultRunSpec(r.expected[phase]["spec"].(map[string]any))
	if phase == "publish" {
		expectedRun["pipelineSpec"].(map[string]any)["tasks"].([]any)[0].(map[string]any)["params"].([]any)[0].(map[string]any)["value"] = runUID
	}
	if !reflect.DeepEqual(runStatus["pipelineSpec"], expectedRun["pipelineSpec"]) {
		return out, ErrBuildTransport
	}
	// A terminal API object alone is insufficient if its original node/boot or
	// physical storage custody changed or its kubelet lease is stale.
	if e := r.inspectBuildSlot(ctx); e != nil {
		return out, e
	}
	now := time.Now()
	requestSHA := r.compiled.Plan.BuildRequestSHA256
	if phase == "publish" {
		requestSHA = r.compiled.Plan.PublishRequestSHA256
	}
	out.Termination = building.Termination{Version: "ai-sdlc.build-termination/v1", Phase: phase, RequestSHA256: requestSHA, PipelineRunUID: runUID, TaskRunUID: taskUID, PodUID: podUID, NodeUID: r.slot.NodeUID, NodeBootID: r.slot.NodeBootID, SlotDigest: r.slot.Digest(), PodSpecSHA256: specDigest, Containers: exits, ObservedAt: now.UnixMilli()}
	if e := out.Termination.Validate(r.command, r.compiled.Plan, r.slot, now); e != nil {
		return BuildObservation{}, ErrBuildTransport
	}
	if phase == "publish" {
		out.Results, e = r.publicationResults(runStatus, taskStatus, runUID)
		if e != nil {
			return BuildObservation{}, e
		}
	}
	cached := out.Termination
	cached.Containers = append([]building.ContainerExit(nil), out.Termination.Containers...)
	r.completed[phase] = cached
	out.Complete = true
	return out, nil
}
func (r *BuildResources) publicationResults(run, task map[string]any, runUID string) (map[string]string, error) {
	decode := func(obj map[string]any) (map[string]string, error) {
		rows, ok := obj["results"].([]any)
		if !ok || len(rows) != 6 {
			return nil, ErrBuildTransport
		}
		out := map[string]string{}
		for _, raw := range rows {
			row, ok := raw.(map[string]any)
			if !ok {
				return nil, ErrBuildTransport
			}
			name, ok := row["name"].(string)
			if !ok {
				return nil, ErrBuildTransport
			}
			value, ok := row["value"].(string)
			if !ok || len(value) > 512 {
				return nil, ErrBuildTransport
			}
			if _, seen := out[name]; seen {
				return nil, ErrBuildTransport
			}
			out[name] = value
		}
		return out, nil
	}
	results, e := decode(run)
	if e != nil {
		return nil, e
	}
	taskResults, e := decode(task)
	if e != nil || !reflect.DeepEqual(results, taskResults) {
		return nil, ErrBuildTransport
	}
	if results["IMAGE_URL"] != building.Registry || !runtimeImageDigest.MatchString(results["IMAGE_DIGEST"]) || results["CHAINS-GIT_URL"] != "https://github.com/os-santiago/saferus" || results["CHAINS-GIT_COMMIT"] != r.command.SourceSHA || results["DEFINITION_DIGEST"] != "sha256:"+r.compiled.Plan.DefinitionSHA256 || results["RUN_UID"] != runUID {
		return nil, ErrBuildTransport
	}
	return results, nil
}

// Other Pods referencing the slot are never ignored because their labels differ.
// The only allowed other phase is the original, already completed builder, whose
// full spec and terminal process identities are independently re-observed.
func (r *BuildResources) checkSlotOccupants(pods []map[string]any, phase, runUID string) error {
	for _, pod := range pods {
		spec, ok := pod["spec"].(map[string]any)
		if !ok {
			return ErrBuildTransport
		}
		volumes, ok := spec["volumes"].([]any)
		if !ok {
			continue
		}
		uses := false
		for _, raw := range volumes {
			v, ok := raw.(map[string]any)
			if !ok {
				return ErrBuildTransport
			}
			if pvc, ok := v["persistentVolumeClaim"].(map[string]any); ok && pvc["claimName"] == r.slot.PVC {
				uses = true
			}
		}
		if !uses {
			continue
		}
		if buildLabel(pod, "tekton.dev/pipelineRunUID") == runUID || buildLabel(pod, "tekton.dev/pipelineRun") == r.runName(phase) {
			continue
		}
		// A publisher can coexist only with the stopped original builder.
		completed, known := r.completed["build"]
		pins := r.phasePins["build"]
		if phase != "publish" || !known || pins == nil || pins.PodUID == "" || completed.PodUID != pins.PodUID {
			return ErrBuildTransport
		}
		uid, _, _, e := r.checkOwnedBuildPod(marshalObject(pod), "build", r.knownRuns["build"], pins.TaskUID)
		if e != nil || uid != pins.PodUID {
			return ErrBuildTransport
		}
		status, ok := pod["status"].(map[string]any)
		if !ok || status["phase"] != "Succeeded" {
			return ErrBuildTransport
		}
		_, done, e := inspectContainerStates(spec, status, pins)
		if e != nil || !done {
			return ErrBuildTransport
		}
	}
	return nil
}
