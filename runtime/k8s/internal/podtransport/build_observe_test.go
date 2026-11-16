package podtransport

import (
	"context"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"net/http"
	"strings"
	"testing"
	"time"
)

func syntheticBuildChildren(r *BuildResources, phase, runUID string) (map[string]any, map[string]any) {
	runSpec := defaultRunSpec(r.expected[phase]["spec"].(map[string]any))
	taskSpec := cloneObject(runSpec["pipelineSpec"].(map[string]any)["tasks"].([]any)[0].(map[string]any)["taskSpec"].(map[string]any))
	delete(taskSpec, "metadata")
	delete(taskSpec, "spec")
	ts := map[string]any{"taskSpec": taskSpec, "podTemplate": runSpec["taskRunTemplate"].(map[string]any)["podTemplate"], "serviceAccountName": phase, "timeout": runSpec["timeouts"].(map[string]any)["pipeline"]}
	if phase == "publish" {
		ts["params"] = []any{map[string]any{"name": "run-uid", "value": runUID}}
	}
	meta := cloneObject(r.expected[phase]["metadata"].(map[string]any))
	meta["name"] = r.taskName(phase)
	meta["uid"] = fixtureOtherUID
	labels := meta["labels"].(map[string]any)
	for k, v := range map[string]any{"app.kubernetes.io/managed-by": "tekton-pipelines", "tekton.dev/memberOf": "tasks", "tekton.dev/pipeline": r.runName(phase), "tekton.dev/pipelineRun": r.runName(phase), "tekton.dev/pipelineRunUID": runUID, "tekton.dev/pipelineTask": phase} {
		labels[k] = v
	}
	meta["annotations"].(map[string]any)["pipeline.tekton.dev/release"] = tektonRelease
	meta["ownerReferences"] = []any{map[string]any{"apiVersion": "tekton.dev/v1", "kind": "PipelineRun", "name": r.runName(phase), "uid": runUID, "controller": true, "blockOwnerDeletion": true}}
	task := map[string]any{"apiVersion": "tekton.dev/v1", "kind": "TaskRun", "metadata": meta, "spec": ts}
	pm := cloneObject(meta)
	pm["name"] = r.taskName(phase) + "-pod"
	pm["uid"] = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	pm["labels"].(map[string]any)["tekton.dev/taskRun"] = r.taskName(phase)
	pm["labels"].(map[string]any)["tekton.dev/taskRunUID"] = fixtureOtherUID
	pm["ownerReferences"] = []any{map[string]any{"apiVersion": "tekton.dev/v1", "kind": "TaskRun", "name": r.taskName(phase), "uid": fixtureOtherUID, "controller": true, "blockOwnerDeletion": true}}
	raw, _ := buildPodTemplates.ReadFile("build_templates/" + phase + "-v1.json")
	var template map[string]any
	_ = json.Unmarshal(raw, &template)
	spec, e := r.expectedBuildPodSpec(phase, runUID, template)
	if e != nil {
		panic(e)
	}
	return task, map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": pm, "spec": spec}
}
func TestBuildObserverRetainsOriginalChildrenAndRejectsExtraWriters(t *testing.T) {
	for _, which := range []string{"pending", "initial-list-race", "task-replaced", "pod-replaced", "task-missing", "pod-missing", "extra-task", "extra-pod", "foreign-slot-writer", "run-replaced", "failed", "pagination", "expired"} {
		t.Run(which, func(t *testing.T) {
			var task, pod map[string]any
			step := 0
			taskLists := 0
			r, _, _ := buildResourceFixture(t, func(r *BuildResources, w http.ResponseWriter, req *http.Request) {
				var value any
				switch req.URL.Path {
				case runPath(r.runName("build")):
					run := runResponse(r, "build")
					if step == 1 && which == "run-replaced" {
						run["metadata"].(map[string]any)["uid"] = fixtureOtherUID
					}
					if step == 1 && which == "failed" {
						run["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Succeeded", "status": "False"}}}
					}
					value = run
				case "/apis/tekton.dev/v1/namespaces/ai-sdlc-builds/taskruns":
					tasks := []any{cloneObject(task)}
					taskLists++
					if which == "initial-list-race" && taskLists == 1 {
						tasks = []any{}
					}
					if step == 1 {
						switch which {
						case "task-replaced":
							tasks[0].(map[string]any)["metadata"].(map[string]any)["uid"] = "dddddddd-dddd-dddd-dddd-dddddddddddd"
						case "task-missing":
							tasks = []any{}
						case "extra-task":
							tasks = append(tasks, cloneObject(task))
						}
					}
					value = map[string]any{"metadata": map[string]any{}, "items": tasks}
				case "/api/v1/namespaces/ai-sdlc-builds/pods":
					pods := []any{cloneObject(pod)}
					metadata := map[string]any{}
					if step == 1 {
						switch which {
						case "pod-replaced":
							pods[0].(map[string]any)["metadata"].(map[string]any)["uid"] = "dddddddd-dddd-dddd-dddd-dddddddddddd"
						case "pod-missing":
							pods = []any{}
						case "extra-pod":
							pods = append(pods, cloneObject(pod))
						case "foreign-slot-writer":
							foreign := cloneObject(pod)
							foreign["metadata"] = map[string]any{"name": "foreign"}
							pods = append(pods, foreign)
						case "pagination":
							metadata["continue"] = "more"
						}
					}
					value = map[string]any{"metadata": metadata, "items": pods}
				default:
					t.Error("unexpected API path", req.URL.Path)
					w.WriteHeader(500)
					return
				}
				completeSyntheticList(req.URL.Path, value)
				_ = json.NewEncoder(w).Encode(value)
			})
			task, pod = syntheticBuildChildren(r, "build", fixtureConfigUID)
			r.knownRuns["build"] = fixtureConfigUID
			first, e := r.observePhase(context.Background(), "build", fixtureConfigUID)
			if e != nil || first.Complete {
				t.Fatal("initial pending observation", e)
			}
			step = 1
			if which == "expired" {
				r.compiled.ExpiresAt = time.Now().Add(-time.Second)
			}
			next, e := r.observePhase(context.Background(), "build", fixtureConfigUID)
			if which == "pending" || which == "initial-list-race" {
				if e != nil || next.Complete {
					t.Fatal("legitimate pending denied", e)
				}
			} else if e == nil {
				t.Fatal("changed or incomplete original identity accepted")
			}
		})
	}
}
func TestBuildPublicationResultsRequireAllExactOriginalValues(t *testing.T) {
	r, _, _ := buildResourceFixture(t, func(_ *BuildResources, _ http.ResponseWriter, _ *http.Request) { t.Fatal("unexpected network") })
	base := map[string]string{"IMAGE_URL": building.Registry, "IMAGE_DIGEST": "sha256:" + strings.Repeat("a", 64), "CHAINS-GIT_URL": "https://github.com/os-santiago/saferus", "CHAINS-GIT_COMMIT": r.command.SourceSHA, "DEFINITION_DIGEST": "sha256:" + r.compiled.Plan.DefinitionSHA256, "RUN_UID": fixtureConfigUID}
	rows := []any{}
	for name, value := range base {
		rows = append(rows, map[string]any{"name": name, "value": value})
	}
	object := map[string]any{"results": rows}
	if _, e := r.publicationResults(object, object, fixtureConfigUID); e != nil {
		t.Fatal("valid synthetic results denied", e)
	}
	for _, which := range []string{"mismatch", "missing", "extra", "duplicate", "type", "source", "uid"} {
		t.Run(which, func(t *testing.T) {
			obj := cloneObject(object)
			items := obj["results"].([]any)
			switch which {
			case "mismatch":
				items[0].(map[string]any)["value"] = "wrong"
			case "missing":
				obj["results"] = items[:5]
			case "extra":
				obj["results"] = append(items, map[string]any{"name": "extra", "value": "x"})
			case "duplicate":
				items[1] = items[0]
			case "type":
				items[0].(map[string]any)["value"] = float64(2)
			case "source", "uid":
				for _, raw := range items {
					row := raw.(map[string]any)
					if (which == "source" && row["name"] == "CHAINS-GIT_COMMIT") || (which == "uid" && row["name"] == "RUN_UID") {
						row["value"] = "changed"
					}
				}
			}
			if _, e := r.publicationResults(obj, obj, fixtureConfigUID); e == nil {
				t.Fatal("invalid results accepted")
			}
		})
	}
}

func completeSyntheticList(path string, value any) {
	obj, ok := value.(map[string]any)
	if !ok {
		return
	}
	if _, list := obj["items"]; !list {
		return
	}
	version, kind, _, known := buildListType(path)
	if known {
		obj["apiVersion"] = version
		obj["kind"] = kind
	}
}
func TestBuildListsNormalizeOnlyKnownAuthenticatedCollectionTypes(t *testing.T) {
	for _, which := range []string{"omitted-core-type", "explicit-core-type", "wrong-list-kind", "wrong-list-version", "wrong-item-kind", "wrong-item-version", "partial-item-type", "unknown-path"} {
		t.Run(which, func(t *testing.T) {
			r, _, _ := buildResourceFixture(t, func(_ *BuildResources, w http.ResponseWriter, req *http.Request) {
				item := map[string]any{"metadata": map[string]any{"uid": fixtureOtherUID}, "spec": map[string]any{}}
				obj := map[string]any{"apiVersion": "v1", "kind": "PodList", "metadata": map[string]any{}, "items": []any{item}}
				switch which {
				case "explicit-core-type":
					item["apiVersion"] = "v1"
					item["kind"] = "Pod"
				case "wrong-list-kind":
					obj["kind"] = "TaskRunList"
				case "wrong-list-version":
					obj["apiVersion"] = "unknown/v1"
				case "wrong-item-kind":
					item["apiVersion"] = "v1"
					item["kind"] = "Secret"
				case "wrong-item-version":
					item["apiVersion"] = "unknown/v1"
					item["kind"] = "Pod"
				case "partial-item-type":
					item["kind"] = "Pod"
				}
				_ = json.NewEncoder(w).Encode(obj)
			})
			path := "/api/v1/namespaces/ai-sdlc-builds/pods"
			if which == "unknown-path" {
				path = "/api/v1/namespaces/foreign/pods"
			}
			items, e := r.listBuildObjects(context.Background(), path)
			if which == "omitted-core-type" || which == "explicit-core-type" {
				if e != nil || len(items) != 1 || items[0]["apiVersion"] != "v1" || items[0]["kind"] != "Pod" {
					t.Fatal("valid typed collection rejected", e)
				}
			} else if e == nil {
				t.Fatal("untrusted type normalization accepted")
			}
		})
	}
}

func TestBuildTaskAllowsOnlyTypedResultsDeletionObservation(t *testing.T) {
	r, _, _ := buildResourceFixture(t, func(_ *BuildResources, _ http.ResponseWriter, _ *http.Request) { t.Fatal("unexpected HTTP") })
	for _, value := range []any{"true", "false", "yes", true} {
		task, pod := syntheticBuildChildren(r, "build", fixtureConfigUID)
		task["metadata"].(map[string]any)["annotations"].(map[string]any)["results.tekton.dev/childReadyForDeletion"] = value
		_, _, e := r.checkTask(marshalObject(task), "build", fixtureConfigUID)
		if value == "true" || value == "false" {
			if e != nil {
				t.Fatal("valid Results observation rejected")
			}
		} else if e == nil {
			t.Fatal("invalid Results observation accepted")
		}
		pod["metadata"].(map[string]any)["annotations"].(map[string]any)["results.tekton.dev/childReadyForDeletion"] = value
		if _, _, _, e := r.checkOwnedBuildPod(marshalObject(pod), "build", fixtureConfigUID, fixtureOtherUID); e == nil {
			t.Fatal("Task-only annotation accepted on Pod")
		}
	}
}
