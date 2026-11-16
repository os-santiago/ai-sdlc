package podtransport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
)

// Synthetic TLS responses exercise composition and failure behavior. The
// separate opt-in slot fixture verifies actual API serialization and custody.
func syntheticSlotResponse(r *BuildResources, path string) map[string]any {
	s := r.slot
	object := func(uid string) map[string]any { return map[string]any{"metadata": map[string]any{"uid": uid}} }
	switch path {
	case "/api/v1/namespaces/kube-system":
		return object(s.ClusterUID)
	case "/api/v1/namespaces/" + s.Namespace:
		return object(s.NamespaceUID)
	case "/api/v1/namespaces/" + s.Namespace + "/persistentvolumeclaims/" + s.PVC:
		o := object(s.PVCUID)
		o["spec"] = map[string]any{"accessModes": []any{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "4Gi"}}, "storageClassName": "", "volumeMode": "Filesystem", "volumeName": "fixture-volume"}
		o["status"] = map[string]any{"phase": "Bound"}
		return o
	case "/api/v1/persistentvolumes/fixture-volume":
		o := object(s.PVUID)
		o["spec"] = map[string]any{"accessModes": []any{"ReadWriteOnce"}, "capacity": map[string]any{"storage": "4Gi"}, "claimRef": map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "name": s.PVC, "namespace": s.Namespace, "uid": s.PVCUID}, "persistentVolumeReclaimPolicy": "Retain", "volumeMode": "Filesystem", "local": map[string]any{"path": "/var/local/ai-sdlc/build-slots/" + s.StorageIdentitySHA256}, "nodeAffinity": map[string]any{"required": map[string]any{"nodeSelectorTerms": []any{map[string]any{"matchExpressions": []any{map[string]any{"key": "kubernetes.io/hostname", "operator": "In", "values": []any{s.Node}}}}}}}}
		return o
	case "/api/v1/nodes/" + s.Node:
		o := object(s.NodeUID)
		o["status"] = map[string]any{"nodeInfo": map[string]any{"bootID": s.NodeBootID}, "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
		return o
	case "/apis/coordination.k8s.io/v1/namespaces/kube-node-lease/leases/" + s.Node:
		o := object(fixtureOtherUID)
		o["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"kind": "Node", "name": s.Node, "uid": s.NodeUID}}
		o["spec"] = map[string]any{"holderIdentity": s.Node, "renewTime": time.Now().UTC().Format(time.RFC3339Nano)}
		return o
	}
	return nil
}
func syntheticSucceededObjects(r *BuildResources, phase string) (map[string]any, map[string]any, map[string]any) {
	run := runResponse(r, phase)
	task, pod := syntheticBuildChildren(r, phase, fixtureConfigUID)
	conditions := []any{map[string]any{"type": "Succeeded", "status": "True"}}
	pipeline := cloneObject(defaultRunSpec(r.expected[phase]["spec"].(map[string]any))["pipelineSpec"].(map[string]any))
	if phase == "publish" {
		pipeline["tasks"].([]any)[0].(map[string]any)["params"].([]any)[0].(map[string]any)["value"] = fixtureConfigUID
	}
	run["status"] = map[string]any{"conditions": conditions, "pipelineSpec": pipeline, "childReferences": []any{map[string]any{"apiVersion": "tekton.dev/v1", "kind": "TaskRun", "name": r.taskName(phase), "pipelineTaskName": phase}}}
	task["status"] = map[string]any{"conditions": conditions, "podName": r.taskName(phase) + "-pod", "taskSpec": r.expectedResolvedTask(phase, fixtureConfigUID)}
	spec := pod["spec"].(map[string]any)
	spec["nodeName"] = r.slot.Node
	status := map[string]any{"phase": "Succeeded"}
	counter := 0
	for _, fields := range [][2]string{{"initContainers", "initContainerStatuses"}, {"containers", "containerStatuses"}} {
		rows := []any{}
		for _, raw := range spec[fields[0]].([]any) {
			c := raw.(map[string]any)
			counter++
			id := fmt.Sprintf("containerd://%064x", counter)
			image, _, _ := normalizedImageID(c["image"].(string))
			stamp := time.UnixMilli(r.command.IssuedAt).UTC().Format(time.RFC3339Nano)
			rows = append(rows, map[string]any{"name": c["name"], "containerID": id, "imageID": image, "restartCount": float64(0), "lastState": map[string]any{}, "state": map[string]any{"terminated": map[string]any{"containerID": id, "exitCode": float64(0), "startedAt": stamp, "finishedAt": stamp}}})
		}
		status[fields[1]] = rows
	}
	pod["status"] = status
	if phase == "publish" {
		values := map[string]string{"IMAGE_URL": building.Registry, "IMAGE_DIGEST": "sha256:" + strings.Repeat("a", 64), "CHAINS-GIT_URL": "https://github.com/os-santiago/saferus", "CHAINS-GIT_COMMIT": r.command.SourceSHA, "DEFINITION_DIGEST": "sha256:" + r.compiled.Plan.DefinitionSHA256, "RUN_UID": fixtureConfigUID}
		rows := []any{}
		for k, v := range values {
			rows = append(rows, map[string]any{"name": k, "value": v})
		}
		run["status"].(map[string]any)["results"] = rows
		task["status"].(map[string]any)["results"] = rows
	}
	return run, task, pod
}
func TestBuildObserverRequiresCompleteFreshTerminalEvidence(t *testing.T) {
	for _, phase := range []string{"build", "publish"} {
		for _, which := range []string{"success", "stale-lease", "node-boot", "node-unready", "foreign-volume", "missing-init", "restart", "failed-exit", "run-pending", "task-pending", "run-pipeline", "task-resolved", "child-ref", "pod-node", "result-drift"} {
			if phase == "build" && which == "result-drift" {
				continue
			}
			t.Run(phase+"/"+which, func(t *testing.T) {
				var run, task, pod map[string]any
				r, _, _ := buildResourceFixture(t, func(r *BuildResources, w http.ResponseWriter, req *http.Request) {
					var obj map[string]any
					switch req.URL.Path {
					case runPath(r.runName(phase)):
						obj = run
					case "/apis/tekton.dev/v1/namespaces/ai-sdlc-builds/taskruns":
						obj = map[string]any{"metadata": map[string]any{}, "items": []any{task}}
					case "/api/v1/namespaces/ai-sdlc-builds/pods":
						obj = map[string]any{"metadata": map[string]any{}, "items": []any{pod}}
					default:
						obj = syntheticSlotResponse(r, req.URL.Path)
						if obj != nil {
							switch which {
							case "stale-lease":
								if strings.Contains(req.URL.Path, "/leases/") {
									obj["spec"].(map[string]any)["renewTime"] = time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
								}
							case "node-boot":
								if strings.Contains(req.URL.Path, "/nodes/") {
									obj["status"].(map[string]any)["nodeInfo"].(map[string]any)["bootID"] = fixtureOtherUID
								}
							case "node-unready":
								if strings.Contains(req.URL.Path, "/nodes/") {
									obj["status"].(map[string]any)["conditions"].([]any)[0].(map[string]any)["status"] = "False"
								}
							case "foreign-volume":
								if strings.Contains(req.URL.Path, "/persistentvolumes/") {
									obj["spec"].(map[string]any)["local"].(map[string]any)["path"] = "/foreign"
								}
							}
						}
					}
					if obj == nil {
						t.Error("unexpected path", req.URL.Path)
						w.WriteHeader(500)
						return
					}
					_ = json.NewEncoder(w).Encode(obj)
				})
				run, task, pod = syntheticSucceededObjects(r, phase)
				r.knownRuns[phase] = fixtureConfigUID
				switch which {
				case "missing-init":
					delete(pod["status"].(map[string]any), "initContainerStatuses")
				case "restart":
					pod["status"].(map[string]any)["containerStatuses"].([]any)[0].(map[string]any)["restartCount"] = float64(1)
				case "failed-exit":
					pod["status"].(map[string]any)["containerStatuses"].([]any)[0].(map[string]any)["state"].(map[string]any)["terminated"].(map[string]any)["exitCode"] = float64(1)
				case "run-pending":
					run["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Succeeded", "status": "Unknown"}}
				case "task-pending":
					task["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Succeeded", "status": "Unknown"}}
				case "run-pipeline":
					run["status"].(map[string]any)["pipelineSpec"] = map[string]any{}
				case "task-resolved":
					task["status"].(map[string]any)["taskSpec"] = map[string]any{}
				case "child-ref":
					run["status"].(map[string]any)["childReferences"] = []any{}
				case "pod-node":
					pod["spec"].(map[string]any)["nodeName"] = "foreign"
				case "result-drift":
					run["status"].(map[string]any)["results"] = []any{}
				}
				out, e := r.observePhase(context.Background(), phase, fixtureConfigUID)
				if which == "success" {
					if e != nil || !out.Complete {
						t.Fatal("valid synthetic terminal denied", e)
					}
					if _, ok := r.completed[phase]; !ok {
						t.Fatal("completion not retained")
					}
				} else {
					if out.Complete {
						t.Fatal("unproven terminal accepted")
					}
					if _, ok := r.completed[phase]; ok {
						t.Fatal("unproven completion retained")
					}
					if e == nil && which != "run-pending" && which != "task-pending" && which != "missing-init" {
						t.Fatal("invalid observation not rejected")
					}
				}
			})
		}
	}
}

func TestBuildObservationFailurePermanentlyInvalidatesPublicationContext(t *testing.T) {
	for _, failure := range []string{"pending-regression", "failed"} {
		t.Run(failure, func(t *testing.T) {
			var run, task, pod map[string]any
			r, calls, _ := buildResourceFixture(t, func(r *BuildResources, w http.ResponseWriter, req *http.Request) {
				if req.Method != "GET" {
					t.Error("invalid context attempted mutation")
					w.WriteHeader(500)
					return
				}
				var obj map[string]any
				switch req.URL.Path {
				case runPath(r.runName("build")):
					obj = run
				case "/apis/tekton.dev/v1/namespaces/ai-sdlc-builds/taskruns":
					obj = map[string]any{"metadata": map[string]any{}, "items": []any{task}}
				case "/api/v1/namespaces/ai-sdlc-builds/pods":
					obj = map[string]any{"metadata": map[string]any{}, "items": []any{pod}}
				default:
					obj = syntheticSlotResponse(r, req.URL.Path)
				}
				if obj == nil {
					t.Error("unknown API path")
					w.WriteHeader(500)
					return
				}
				_ = json.NewEncoder(w).Encode(obj)
			})
			run, task, pod = syntheticSucceededObjects(r, "build")
			r.knownRuns["build"] = fixtureConfigUID
			out, e := r.observePhase(context.Background(), "build", fixtureConfigUID)
			if e != nil || !out.Complete {
				t.Fatal("initial completion", e)
			}
			original := r.completed["build"].Containers[0].ContainerID
			out.Termination.Containers[0].ContainerID = "caller mutation"
			if r.completed["build"].Containers[0].ContainerID != original {
				t.Fatal("caller can mutate retained completion")
			}
			status := "Unknown"
			if failure == "failed" {
				status = "False"
			}
			run["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Succeeded", "status": status}}
			if _, e := r.observePhase(context.Background(), "build", fixtureConfigUID); e == nil {
				t.Fatal("completion regression did not invalidate context")
			}
			before := calls.Load()
			if _, e := r.postRun(context.Background(), "publish"); e == nil || calls.Load() != before {
				t.Fatal("stale completion authorized publication")
			}
			run["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Succeeded", "status": "True"}}
			if _, e := r.observePhase(context.Background(), "build", fixtureConfigUID); e == nil || calls.Load() != before {
				t.Fatal("invalidated context resumed")
			}
		})
	}
}
