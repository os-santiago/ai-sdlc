package podtransport

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const fixtureConfigUID = "11111111-1111-4111-8111-111111111111"
const fixtureOtherUID = "33333333-3333-4333-8333-333333333333"

func resourcesFixture(t *testing.T, handler func(*Resources, core.Pod, http.ResponseWriter, *http.Request)) (*Resources, *atomic.Int32) {
	t.Helper()
	b, pod, _ := bindingFixture(t)
	b.PodUID = ""
	var resources *Resources
	calls := &atomic.Int32{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 32) {
			t.Error("explicit authorization missing")
			w.WriteHeader(401)
			return
		}
		handler(resources, *pod.DeepCopy(), w, r)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(Config{APIServer: server.URL, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), BearerToken: strings.Repeat("x", 32)})
	if err != nil {
		t.Fatal(err)
	}
	resources, err = PlanResources(client, b)
	if err != nil {
		t.Fatal(err)
	}
	return resources, calls
}
func configResponse(r *Resources) core.ConfigMap {
	c := *r.config.DeepCopy()
	c.UID = types.UID(fixtureConfigUID)
	return c
}
func writeResource(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func TestResourceConfigCreateRejectsMutationAndNeverRetries(t *testing.T) {
	for _, which := range []string{"success", "data", "mutable", "uid", "label", "binary", "owner", "finalizer", "conflict", "unknown", "oversized", "lost-response", "redirect"} {
		t.Run(which, func(t *testing.T) {
			r, calls := resourcesFixture(t, func(resources *Resources, _ core.Pod, w http.ResponseWriter, request *http.Request) {
				if request.Method != "POST" || request.URL.Path != "/api/v1/namespaces/"+resources.binding.Namespace+"/configmaps" {
					t.Error("unexpected create path")
				}
				var sent core.ConfigMap
				if json.NewDecoder(request.Body).Decode(&sent) != nil || sent.Name != resources.config.Name || sent.Immutable == nil || !*sent.Immutable {
					t.Error("wrong fixed body")
				}
				c := configResponse(resources)
				switch which {
				case "data":
					c.Data["assignment.json"] = "forged"
				case "mutable":
					value := false
					c.Immutable = &value
				case "uid":
					c.UID = "invalid"
				case "label":
					c.Labels["injected"] = "true"
				case "binary":
					c.BinaryData = map[string][]byte{"extra": []byte("x")}
				case "owner":
					c.OwnerReferences = []meta.OwnerReference{{UID: types.UID(fixtureOtherUID)}}
				case "finalizer":
					c.Finalizers = []string{"fixture/finalizer"}
				case "conflict":
					w.WriteHeader(409)
					return
				case "unknown":
					w.WriteHeader(201)
					_, _ = w.Write([]byte(`{"unexpected":true}`))
					return
				case "oversized":
					w.WriteHeader(201)
					_, _ = w.Write([]byte(strings.Repeat("x", maxPodBytes+1)))
					return
				case "lost-response":
					connection, _, _ := w.(http.Hijacker).Hijack()
					connection.Close()
					return
				case "redirect":
					w.Header().Set("Location", "/escape")
					w.WriteHeader(307)
					return
				}
				writeResource(w, 201, c)
			})
			uid, err := r.CreateConfig(context.Background())
			if which == "success" {
				if err != nil || uid != fixtureConfigUID {
					t.Fatal("valid config rejected")
				}
			} else if err == nil || uid != "" {
				t.Fatal("unsafe config accepted")
			}
			if calls.Load() != 1 {
				t.Fatal("mutating create retried")
			}
		})
	}
}
func TestResourcePodCreateChecksConfigBeforeAndAfter(t *testing.T) {
	for _, which := range []string{"success", "config-replaced-before", "config-replaced-after", "spec", "owner", "uid", "conflict", "lost-response"} {
		t.Run(which, func(t *testing.T) {
			var posts, gets atomic.Int32
			r, _ := resourcesFixture(t, func(resources *Resources, pod core.Pod, w http.ResponseWriter, request *http.Request) {
				if request.Method == "GET" {
					n := gets.Add(1)
					c := configResponse(resources)
					if which == "config-replaced-before" || (which == "config-replaced-after" && n == 2) {
						c.UID = types.UID(fixtureOtherUID)
					}
					writeResource(w, 200, c)
					return
				}
				if request.Method != "POST" || !strings.HasSuffix(request.URL.Path, "/pods") {
					t.Error("unexpected Pod operation")
				}
				posts.Add(1)
				switch which {
				case "spec":
					pod.Spec.HostNetwork = true
				case "owner":
					pod.OwnerReferences = []meta.OwnerReference{{UID: types.UID(fixtureOtherUID)}}
				case "uid":
					pod.UID = "invalid"
				case "conflict":
					w.WriteHeader(409)
					return
				case "lost-response":
					connection, _, _ := w.(http.Hijacker).Hijack()
					connection.Close()
					return
				}
				writeResource(w, 201, pod)
			})
			uid, err := r.CreatePod(context.Background(), fixtureConfigUID)
			if which == "success" {
				if err != nil || uid == "" || gets.Load() != 2 {
					t.Fatal("valid Pod rejected")
				}
			} else if err == nil || uid != "" {
				t.Fatal("unsafe Pod accepted")
			}
			if posts.Load() > 1 || (which == "config-replaced-before" && posts.Load() != 0) {
				t.Fatal("unsafe or retried Pod creation")
			}
		})
	}
}
func TestResourceWaitBindsExactRunningPod(t *testing.T) {
	for _, which := range []string{"success", "replacement", "mutation", "failed", "expired"} {
		t.Run(which, func(t *testing.T) {
			var uid string
			r, calls := resourcesFixture(t, func(_ *Resources, pod core.Pod, w http.ResponseWriter, request *http.Request) {
				if request.Method != "GET" {
					t.Error("readiness mutated resource")
				}
				switch which {
				case "replacement":
					pod.UID = types.UID(fixtureOtherUID)
				case "mutation":
					pod.Spec.HostNetwork = true
				case "failed":
					pod.Status.Phase = core.PodFailed
				}
				writeResource(w, 200, pod)
			})
			_, pod, _ := bindingFixture(t)
			uid = string(pod.UID)
			if which == "expired" {
				r.expires = time.Now().Add(-time.Second)
			}
			session, err := r.WaitRunning(context.Background(), uid)
			if which == "success" {
				if err != nil || session == nil || calls.Load() != 2 {
					t.Fatal("running Pod not bound")
				}
			} else if err == nil || session != nil {
				t.Fatal("unsafe readiness accepted")
			}
			if which == "expired" && calls.Load() != 0 {
				t.Fatal("expired readiness contacted API")
			}
		})
	}
}
func TestResourceDeleteUsesUIDAndObservesRemoval(t *testing.T) {
	for _, which := range []string{"pod", "config", "expired", "replacement", "conflict", "still-present", "missing"} {
		t.Run(which, func(t *testing.T) {
			var deletes, gets atomic.Int32
			r, _ := resourcesFixture(t, func(resources *Resources, pod core.Pod, w http.ResponseWriter, request *http.Request) {
				if request.Method == "DELETE" {
					deletes.Add(1)
					var options meta.DeleteOptions
					if json.NewDecoder(request.Body).Decode(&options) != nil || options.Preconditions == nil || options.Preconditions.UID == nil || string(*options.Preconditions.UID) != fixtureConfigUID {
						t.Error("cleanup lacks exact UID precondition")
					}
					if which == "conflict" {
						w.WriteHeader(409)
						return
					}
					if which == "missing" {
						w.WriteHeader(404)
						return
					}
					writeResource(w, 200, meta.Status{Status: "Success"})
					return
				}
				if request.Method != "GET" {
					t.Error("unexpected cleanup operation")
				}
				gets.Add(1)
				if which == "replacement" {
					pod.UID = types.UID(fixtureOtherUID)
					writeResource(w, 200, pod)
					return
				}
				if which == "still-present" {
					pod.UID = types.UID(fixtureConfigUID)
					writeResource(w, 200, pod)
					return
				}
				w.WriteHeader(404)
			})
			if which == "expired" {
				r.expires = time.Now().Add(-time.Hour)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			var err error
			if which == "config" {
				err = r.DeleteConfig(ctx, fixtureConfigUID)
			} else {
				err = r.DeletePod(ctx, fixtureConfigUID)
			}
			bad := which == "replacement" || which == "conflict" || which == "still-present"
			if (err != nil) != bad {
				t.Fatalf("cleanup outcome=%v", err)
			}
			if deletes.Load() != 1 || ((which == "conflict" || which == "missing") && gets.Load() != 0) {
				t.Fatal("cleanup retried or ignored response")
			}
		})
	}
}
func TestResourceCreateExpiryDuringConfigObservationPreventsPod(t *testing.T) {
	var posts atomic.Int32
	r, _ := resourcesFixture(t, func(resources *Resources, _ core.Pod, w http.ResponseWriter, request *http.Request) {
		if request.Method == "POST" {
			posts.Add(1)
		}
		time.Sleep(100 * time.Millisecond)
		writeResource(w, 200, configResponse(resources))
	})
	r.expires = time.Now().Add(30 * time.Millisecond)
	if uid, err := r.CreatePod(context.Background(), fixtureConfigUID); err == nil || uid != "" || posts.Load() != 0 {
		t.Fatal("expired observation reached Pod create")
	}
}

func TestResourceReadinessWaitsForInitiallyStartingContainer(t *testing.T) {
	var observations atomic.Int32
	r, calls := resourcesFixture(t, func(_ *Resources, pod core.Pod, w http.ResponseWriter, _ *http.Request) {
		if observations.Add(1) == 1 {
			for i := range pod.Status.ContainerStatuses {
				status := &pod.Status.ContainerStatuses[i]
				if status.Name == "worker" {
					status.State = core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "ContainerCreating"}}
					status.ContainerID = ""
					status.ImageID = ""
				}
			}
		}
		writeResource(w, 200, pod)
	})
	_, pod, _ := bindingFixture(t)
	session, err := r.WaitRunning(context.Background(), string(pod.UID))
	if err != nil || session == nil || calls.Load() != 3 {
		t.Fatal("healthy initial container creation was rejected")
	}
}

func TestResourceReadinessRejectsUnsafeInitialStates(t *testing.T) {
	for _, which := range []string{"restart", "terminated", "unknown-wait", "running-and-waiting", "wrong-image", "duplicate", "previous-termination"} {
		t.Run(which, func(t *testing.T) {
			r, calls := resourcesFixture(t, func(_ *Resources, pod core.Pod, w http.ResponseWriter, _ *http.Request) {
				status := &pod.Status.ContainerStatuses[0]
				switch which {
				case "restart":
					status.RestartCount = 1
				case "terminated":
					status.State = core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}}
				case "unknown-wait":
					status.State = core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
				case "running-and-waiting":
					status.State.Waiting = &core.ContainerStateWaiting{Reason: "ContainerCreating"}
				case "wrong-image":
					status.ImageID = "sha256:" + strings.Repeat("c", 64)
				case "duplicate":
					pod.Status.ContainerStatuses[1] = pod.Status.ContainerStatuses[0]
				case "previous-termination":
					status.LastTerminationState.Terminated = &core.ContainerStateTerminated{ExitCode: 0}
				}
				writeResource(w, 200, pod)
			})
			_, pod, _ := bindingFixture(t)
			if session, err := r.WaitRunning(context.Background(), string(pod.UID)); err == nil || session != nil || calls.Load() != 1 {
				t.Fatal("unsafe startup state was retried or accepted")
			}
		})
	}
}

func TestRecoveryCleanupRejectsSubstitutionAndUsesUID(t *testing.T) {
	r, calls := resourcesFixture(t, func(_ *Resources, _ core.Pod, w http.ResponseWriter, request *http.Request) {
		if request.Method != "DELETE" || !strings.HasPrefix(request.URL.Path, "/api/v1/namespaces/ai-sdlc-executors/") {
			t.Error("cleanup exceeded fixed surface")
		}
		var options meta.DeleteOptions
		if json.NewDecoder(request.Body).Decode(&options) != nil || options.Preconditions == nil || string(*options.Preconditions.UID) != fixtureConfigUID {
			t.Error("cleanup omitted UID")
		}
		w.WriteHeader(404)
	})
	pod := "executor-" + fixtureConfigUID + "-1"
	config := "assignment-" + strings.Repeat("a", 32)
	for _, namespace := range []string{"ai-sdlc-runtime", "../default", "default"} {
		if err := r.client.CleanupAcknowledged(context.Background(), namespace, pod, config, fixtureConfigUID, fixtureConfigUID); err == nil {
			t.Fatal("unsafe namespace accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid cleanup reached API")
	}
	if err := r.client.CleanupAcknowledged(context.Background(), "ai-sdlc-executors", pod, config, fixtureConfigUID, fixtureConfigUID); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("cleanup did not use both UIDs")
	}
}
