package podtransport

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
)

const fixtureToolchainImage = "registry.example/go@sha256:" + "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// toolchainBindingFixture is bindingFixture with a generation toolchain: the
// running pod has completed both compiled init containers.
func toolchainBindingFixture(t *testing.T) (Binding, core.Pod) {
	t.Helper()
	b, base, _ := bindingFixture(t)
	var a executor.Assignment
	if json.Unmarshal(b.Assignment, &a) != nil {
		t.Fatal("fixture assignment")
	}
	a.Workspace = executor.WorkspaceOfflineTests
	a.Toolchain = &executor.Toolchain{Image: fixtureToolchainImage, Root: "/usr/local/go", Path: []string{"bin"}, Command: []string{"go", "test", "./..."}}
	b.Assignment, _ = json.Marshal(a)
	w, err := executor.Compile(b.Namespace, b.WorkerImage, b.MailboxImage, b.RuntimeClass, b.Assignment, b.Profile, b.ADev, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(w.Pod)
	var p core.Pod
	if json.Unmarshal(raw, &p) != nil {
		t.Fatal("fixture pod decode")
	}
	applyDefaults(&p.Spec)
	p.Spec.NodeName = "fixture-node"
	p.UID = types.UID(b.PodUID)
	p.Status = base.Status
	p.Status.InitContainerStatuses = []core.ContainerStatus{
		terminatedInit(executor.InitInstallRunner, "fixture/mailbox:imported", "fixture/mailbox@sha256:"+strings.Repeat("b", 64), "d", 0),
		terminatedInit(executor.InitCopyToolchain, fixtureToolchainImage, "registry.example/go@sha256:"+strings.Repeat("c", 64), "e", 0),
	}
	return b, p
}

func terminatedInit(name, image, imageID, id string, code int32) core.ContainerStatus {
	container := "containerd://" + strings.Repeat(id, 64)
	return core.ContainerStatus{Name: name, ContainerID: container, Image: image, ImageID: imageID,
		State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: code, Reason: "Completed", ContainerID: container}}}
}

func waitingInit(status *core.ContainerStatus, reason string) {
	status.State = core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: reason}}
	status.ContainerID, status.ImageID = "", ""
}

func runningInit(status *core.ContainerStatus) {
	status.State = core.ContainerState{Running: &core.ContainerStateRunning{}}
}

func initializing(pod *core.Pod) {
	pod.Status.Phase = core.PodPending
	for i := range pod.Status.ContainerStatuses {
		status := &pod.Status.ContainerStatuses[i]
		status.State = core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "PodInitializing"}}
		status.ContainerID, status.ImageID = "", ""
	}
}

func toolchainResources(t *testing.T, handler func(observation int32, pod *core.Pod)) (*Resources, Binding, core.Pod, *atomic.Int32) {
	t.Helper()
	b, pod := toolchainBindingFixture(t)
	plan := b
	plan.PodUID = ""
	calls := &atomic.Int32{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || strings.HasSuffix(r.URL.Path, "/exec") {
			t.Error("readiness mutated or executed")
			w.WriteHeader(500)
			return
		}
		p := pod.DeepCopy()
		handler(calls.Add(1), p)
		writeResource(w, 200, p)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(Config{APIServer: server.URL, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), BearerToken: strings.Repeat("x", 32)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := PlanResources(client, plan)
	if err != nil {
		t.Fatal(err)
	}
	return r, b, pod, calls
}

func TestToolchainPodReadinessFollowsTheCompiledInitSequence(t *testing.T) {
	r, _, _, calls := toolchainResources(t, func(n int32, pod *core.Pod) {
		inits := pod.Status.InitContainerStatuses
		switch n {
		case 1: // not yet reported by the kubelet
			initializing(pod)
			pod.Status.InitContainerStatuses = nil
		case 2:
			initializing(pod)
			waitingInit(&inits[0], "ContainerCreating")
			waitingInit(&inits[1], "PodInitializing")
		case 3:
			initializing(pod)
			runningInit(&inits[0])
			waitingInit(&inits[1], "PodInitializing")
		case 4:
			initializing(pod)
			runningInit(&inits[1])
		case 5:
			initializing(pod)
		}
	})
	if budget := startBudget(r.pod); budget != StartBudget+executor.ToolchainStartSeconds*time.Second {
		t.Fatal("start budget does not cover the toolchain", budget)
	}
	session, err := r.WaitRunning(context.Background(), "22222222-2222-4222-8222-222222222222")
	if err != nil || session == nil || calls.Load() != 7 {
		t.Fatal("compiled init sequence refused", err, calls.Load())
	}
	baseline := session.initInstances()
	if len(baseline) != 2 || baseline[executor.InitCopyToolchain].ID != "containerd://"+strings.Repeat("e", 64) {
		t.Fatal("init containers missing from the session baseline", baseline)
	}
}

func TestToolchainPodReadinessRefusesAnyOtherInitSequence(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(n int32, pod *core.Pod)
		step string
	}{
		"copy-refused-dynamic": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			pod.Status.Phase = core.PodFailed
			pod.Status.InitContainerStatuses[1].State.Terminated.ExitCode = 6
		}, "toolchain-dynamic"},
		"copy-refused-limit": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			pod.Status.Phase = core.PodFailed
			pod.Status.InitContainerStatuses[1].State.Terminated.ExitCode = 5
		}, "toolchain-limit"},
		"copy-other-exit": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			pod.Status.InitContainerStatuses[1].State.Terminated.ExitCode = 9
		}, "toolchain-failed"},
		"copy-signal": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			pod.Status.InitContainerStatuses[1].State.Terminated.ExitCode = 5
			pod.Status.InitContainerStatuses[1].State.Terminated.Signal = 9
		}, "toolchain-failed"},
		"copy-oom": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			pod.Status.InitContainerStatuses[1].State.Terminated.ExitCode = 137
			pod.Status.InitContainerStatuses[1].State.Terminated.Reason = "OOMKilled"
		}, "toolchain-memory"},
		"install-failed": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			pod.Status.InitContainerStatuses[0].State.Terminated.ExitCode = 1
			waitingInit(&pod.Status.InitContainerStatuses[1], "PodInitializing")
		}, "toolchain-install"},
		"restarted": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			pod.Status.InitContainerStatuses[1].RestartCount = 1
		}, "init-identity"},
		"previous-termination": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			runningInit(&pod.Status.InitContainerStatuses[1])
			pod.Status.InitContainerStatuses[1].LastTerminationState.Terminated = &core.ContainerStateTerminated{ExitCode: 1}
		}, "init-identity"},
		"extra-status": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			extra := pod.Status.InitContainerStatuses[1]
			extra.Name = "injected"
			pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses, extra)
		}, "init-count"},
		"missing-status": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			pod.Status.InitContainerStatuses = pod.Status.InitContainerStatuses[:1]
		}, "init-count"},
		"reordered-status": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			s := pod.Status.InitContainerStatuses
			s[0], s[1] = s[1], s[0]
		}, "init-identity"},
		"copy-before-install": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			waitingInit(&pod.Status.InitContainerStatuses[0], "PodInitializing")
			runningInit(&pod.Status.InitContainerStatuses[1])
		}, "init-running"},
		"both-running": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			runningInit(&pod.Status.InitContainerStatuses[0])
			runningInit(&pod.Status.InitContainerStatuses[1])
		}, "init-running"},
		"copy-done-install-waiting": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			waitingInit(&pod.Status.InitContainerStatuses[0], "PodInitializing")
		}, "init-terminated"},
		"worker-during-copy": {func(_ int32, pod *core.Pod) {
			pod.Status.Phase = core.PodPending
			runningInit(&pod.Status.InitContainerStatuses[1])
		}, "containers-before-init"},
		"wrong-copy-image": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			pod.Status.InitContainerStatuses[1].ImageID = "registry.example/go@sha256:" + strings.Repeat("f", 64)
		}, "init-image"},
		"wrong-install-image": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			pod.Status.InitContainerStatuses[0].ImageID = "fixture/worker@sha256:" + strings.Repeat("a", 64)
		}, "init-image"},
		"terminated-id-mismatch": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			pod.Status.InitContainerStatuses[1].State.Terminated.ContainerID = "containerd://" + strings.Repeat("f", 64)
		}, "init-terminated"},
		"terminated-without-id": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			pod.Status.InitContainerStatuses[1].ContainerID = ""
		}, "init-terminated"},
		"waiting-with-id": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			waitingInit(&pod.Status.InitContainerStatuses[1], "PodInitializing")
			pod.Status.InitContainerStatuses[1].ContainerID = "containerd://" + strings.Repeat("e", 64)
		}, "init-created"},
		"pull-error": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			waitingInit(&pod.Status.InitContainerStatuses[1], "ErrImagePull")
		}, "init-waiting"},
		"no-state": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			pod.Status.InitContainerStatuses[1].State = core.ContainerState{}
		}, "init-state"},
		"running-without-init-status": {func(_ int32, pod *core.Pod) {
			pod.Status.InitContainerStatuses = nil
		}, "containers-before-init"},
		"extra-spec-init": {func(_ int32, pod *core.Pod) {
			pod.Spec.InitContainers = append(pod.Spec.InitContainers, pod.Spec.InitContainers[0])
			pod.Spec.InitContainers[2].Name = "injected"
		}, "template"},
		"reordered-spec-init": {func(_ int32, pod *core.Pod) {
			s := pod.Spec.InitContainers
			s[0], s[1] = s[1], s[0]
		}, "template"},
		"missing-spec-init": {func(_ int32, pod *core.Pod) {
			pod.Spec.InitContainers = pod.Spec.InitContainers[:1]
		}, "template"},
		"init-sidecar": {func(_ int32, pod *core.Pod) {
			always := core.ContainerRestartPolicyAlways
			pod.Spec.InitContainers[1].RestartPolicy = &always
		}, "template"},
		"copy-root-changed": {func(_ int32, pod *core.Pod) {
			pod.Spec.InitContainers[1].Command[2] = "/"
		}, "template"},
		"copy-writes-runner": {func(_ int32, pod *core.Pod) {
			pod.Spec.InitContainers[1].VolumeMounts[0].ReadOnly = false
		}, "template"},
	} {
		t.Run(name, func(t *testing.T) {
			r, _, _, calls := toolchainResources(t, c.edit)
			session, err := r.WaitRunning(context.Background(), "22222222-2222-4222-8222-222222222222")
			if err == nil || session != nil || !errors.Is(err, ErrTransport) || calls.Load() != 1 {
				t.Fatal("unsafe init sequence accepted or retried", err, calls.Load())
			}
			// Every refusal names its bounded step.
			if ReadinessStep(err) != c.step || !KnownReadinessStep(c.step) {
				t.Fatal("readiness step", ReadinessStep(err), c.step)
			}
		})
	}
}

func TestToolchainInitContainerIDsAreStable(t *testing.T) {
	// The copy container is observed running as one container and then
	// reported terminated as another.
	r, _, _, _ := toolchainResources(t, func(n int32, pod *core.Pod) {
		if n == 1 {
			initializing(pod)
			runningInit(&pod.Status.InitContainerStatuses[1])
			pod.Status.InitContainerStatuses[1].ContainerID = "containerd://" + strings.Repeat("f", 64)
		}
	})
	if session, err := r.WaitRunning(context.Background(), "22222222-2222-4222-8222-222222222222"); err == nil || session != nil {
		t.Fatal("replaced init container accepted during start")
	}
	// Once an init container has been observed, every later poll must still
	// report it: an empty list or a return to waiting refuses the pod.
	for name, edit := range map[string]func(*core.Pod){
		"empty": func(p *core.Pod) { p.Status.InitContainerStatuses = nil },
		"waiting": func(p *core.Pod) {
			waitingInit(&p.Status.InitContainerStatuses[0], "PodInitializing")
			waitingInit(&p.Status.InitContainerStatuses[1], "PodInitializing")
		},
		"copy-reset": func(p *core.Pod) { waitingInit(&p.Status.InitContainerStatuses[1], "PodInitializing") },
	} {
		t.Run("disappears-"+name, func(t *testing.T) {
			r, _, _, calls := toolchainResources(t, func(n int32, pod *core.Pod) {
				initializing(pod)
				switch n {
				case 1:
					runningInit(&pod.Status.InitContainerStatuses[1])
				default:
					edit(pod)
				}
			})
			if session, err := r.WaitRunning(context.Background(), "22222222-2222-4222-8222-222222222222"); err == nil || session != nil || calls.Load() != 2 || ReadinessStep(err) != "init-unstable" {
				t.Fatal("init container vanished during start and was accepted or misnamed", err, calls.Load())
			}
		})
	}
	// The session's own baseline must be the init containers readiness saw.
	r, _, _, _ = toolchainResources(t, func(n int32, pod *core.Pod) {
		if n == 2 {
			pod.Status.InitContainerStatuses[0] = terminatedInit(executor.InitInstallRunner, "fixture/mailbox:imported", "fixture/mailbox@sha256:"+strings.Repeat("b", 64), "f", 0)
		}
	})
	if session, err := r.WaitRunning(context.Background(), "22222222-2222-4222-8222-222222222222"); err == nil || session != nil || ReadinessStep(err) != "bind-unstable" {
		t.Fatal("bound session with other init containers", err)
	}
	// After binding, any change to an init container stops the session.
	for name, edit := range map[string]func(*core.Pod){
		"replaced": func(p *core.Pod) {
			p.Status.InitContainerStatuses[1] = terminatedInit(executor.InitCopyToolchain, fixtureToolchainImage, "registry.example/go@sha256:"+strings.Repeat("c", 64), "f", 0)
		},
		"restarted": func(p *core.Pod) { p.Status.InitContainerStatuses[1].RestartCount = 1 },
		"removed":   func(p *core.Pod) { p.Status.InitContainerStatuses = nil },
		"failed":    func(p *core.Pod) { p.Status.InitContainerStatuses[1].State.Terminated.ExitCode = 3 },
	} {
		t.Run(name, func(t *testing.T) {
			r, _, _, _ := toolchainResources(t, func(n int32, pod *core.Pod) {
				if n > 2 {
					edit(pod)
				}
			})
			session, err := r.WaitRunning(context.Background(), "22222222-2222-4222-8222-222222222222")
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := session.WorkerExit(context.Background()); err == nil {
				t.Fatal("changed init container accepted after binding")
			}
		})
	}
}

func TestPodWithoutToolchainStillRefusesInitStatuses(t *testing.T) {
	r, _ := resourcesFixture(t, func(_ *Resources, pod core.Pod, w http.ResponseWriter, _ *http.Request) {
		pod.Status.InitContainerStatuses = []core.ContainerStatus{terminatedInit(executor.InitInstallRunner, "fixture/mailbox:imported", "fixture/mailbox@sha256:"+strings.Repeat("b", 64), "d", 0)}
		writeResource(w, 200, pod)
	})
	if startBudget(r.pod) != StartBudget {
		t.Fatal("start budget without a toolchain changed")
	}
	if session, err := r.WaitRunning(context.Background(), "22222222-2222-4222-8222-222222222222"); err == nil || session != nil {
		t.Fatal("init status accepted for a pod without init containers")
	}
}

func TestToolchainPodComposesWithRuntimeClassAdmission(t *testing.T) {
	admission := RuntimeClassAdmission{Overhead: core.ResourceList{core.ResourceCPU: resource.MustParse("250m"), core.ResourceMemory: resource.MustParse("160Mi")},
		NodeSelector: map[string]string{"sandbox": "kata"}, Tolerations: []core.Toleration{{Key: "sandbox", Operator: core.TolerationOpExists, Effect: core.TaintEffectNoSchedule}}}
	for _, admitted := range []bool{true, false} {
		r, _, _, _ := toolchainResources(t, func(_ int32, pod *core.Pod) {
			if !applyAdmission(&pod.Spec, admission) {
				t.Error("fixture admission")
			}
		})
		if admitted {
			r.binding.Admission = admission
		}
		session, err := r.WaitRunning(context.Background(), "22222222-2222-4222-8222-222222222222")
		if admitted && (err != nil || session == nil) {
			t.Fatal("toolchain pod with the observed class admission refused", err)
		}
		if !admitted && err == nil {
			t.Fatal("class additions accepted without the observed admission")
		}
	}
}

func TestReadinessNamesEveryOtherRefusalStep(t *testing.T) {
	for name, c := range map[string]struct {
		respond func(core.Pod, http.ResponseWriter)
		cause   error
	}{
		"status":     {func(pod core.Pod, w http.ResponseWriter) { writeResource(w, 404, pod) }, ErrTransport},
		"permission": {func(_ core.Pod, w http.ResponseWriter) { w.WriteHeader(403) }, ErrPermission},
		"template": {func(pod core.Pod, w http.ResponseWriter) {
			pod.Spec.Containers[0].Image = "fixture/other@sha256:" + strings.Repeat("f", 64)
			writeResource(w, 200, pod)
		}, ErrTransport},
		"phase": {func(pod core.Pod, w http.ResponseWriter) {
			pod.Status.Phase = core.PodFailed
			writeResource(w, 200, pod)
		}, ErrTransport},
		"containers": {func(pod core.Pod, w http.ResponseWriter) {
			pod.Status.ContainerStatuses[0].RestartCount = 1
			writeResource(w, 200, pod)
		}, ErrTransport},
		"request": {func(_ core.Pod, w http.ResponseWriter) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		}, ErrTransport},
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := resourcesFixture(t, func(_ *Resources, pod core.Pod, w http.ResponseWriter, _ *http.Request) { c.respond(pod, w) })
			session, err := r.WaitRunning(context.Background(), "22222222-2222-4222-8222-222222222222")
			if session != nil || !errors.Is(err, c.cause) {
				t.Fatal("refusal changed", err)
			}
			if ReadinessStep(err) != name || !KnownReadinessStep(name) {
				t.Fatal("readiness step", ReadinessStep(err), name)
			}
		})
	}
	// A pod still starting when the caller's deadline ends.
	r, _, _, _ := toolchainResources(t, func(_ int32, pod *core.Pod) {
		initializing(pod)
		runningInit(&pod.Status.InitContainerStatuses[1])
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := r.WaitRunning(ctx, "22222222-2222-4222-8222-222222222222"); !errors.Is(err, ErrTransport) || ReadinessStep(err) != "deadline" {
		t.Fatal("deadline not named", err, ReadinessStep(err))
	}
	if _, err := r.WaitRunning(context.Background(), "not-a-uid"); !errors.Is(err, ErrTransport) || ReadinessStep(err) != "uid" {
		t.Fatal("malformed pod UID not named", err)
	}
	for _, step := range []string{"", "init-other", "toolchain-", "readiness", "bind-x", "request-timeout", "containers-created-x"} {
		if KnownReadinessStep(step) {
			t.Fatal("unbounded readiness step accepted", step)
		}
	}
}

// createdNotStarted is the status the kubelet writes for a container the
// runtime has created but not yet started (convertToAPIContainerStatuses,
// ContainerStateCreated): waiting with no reason, its container ID and image.
func createdNotStarted(status *core.ContainerStatus) {
	status.State = core.ContainerState{Waiting: &core.ContainerStateWaiting{}}
}

func TestToolchainPodAcceptsTheKubeletCreatedState(t *testing.T) {
	// The observed start (battery 25): install-runner completes, then
	// copy-toolchain is reported created (before and after its image ID),
	// then running with the same container ID; one main container is
	// reported created while the other already runs.
	r, _, _, calls := toolchainResources(t, func(n int32, pod *core.Pod) {
		inits := pod.Status.InitContainerStatuses
		switch n {
		case 1:
			initializing(pod)
			waitingInit(&inits[1], "PodInitializing")
		case 2:
			initializing(pod)
			createdNotStarted(&inits[1])
			inits[1].ImageID = ""
		case 3:
			initializing(pod)
			createdNotStarted(&inits[1])
		case 4:
			initializing(pod)
			runningInit(&inits[1])
		case 5:
			pod.Status.Phase = core.PodPending
			createdNotStarted(&pod.Status.ContainerStatuses[0])
			createdNotStarted(&pod.Status.ContainerStatuses[1])
			pod.Status.ContainerStatuses[0].ImageID = ""
		case 6:
			pod.Status.Phase = core.PodRunning
			createdNotStarted(&pod.Status.ContainerStatuses[1])
		}
	})
	session, err := r.WaitRunning(context.Background(), "22222222-2222-4222-8222-222222222222")
	if err != nil || session == nil || calls.Load() != 8 {
		t.Fatal("kubelet created state refused", err, ReadinessStep(err), calls.Load())
	}
	if session.initInstances()[executor.InitCopyToolchain].ID != "containerd://"+strings.Repeat("e", 64) {
		t.Fatal("created container not held in the baseline")
	}
	// A pod without init containers: both main containers reported created
	// first.
	var gets atomic.Int32
	plain, _ := resourcesFixture(t, func(_ *Resources, pod core.Pod, w http.ResponseWriter, _ *http.Request) {
		if gets.Add(1) == 1 {
			pod.Status.Phase = core.PodPending
			createdNotStarted(&pod.Status.ContainerStatuses[0])
			createdNotStarted(&pod.Status.ContainerStatuses[1])
		}
		writeResource(w, 200, pod)
	})
	if session, err := plain.WaitRunning(context.Background(), "22222222-2222-4222-8222-222222222222"); err != nil || session == nil || gets.Load() < 3 {
		t.Fatal("created main containers refused", err, ReadinessStep(err))
	}
}

func TestKubeletCreatedStateIsAcceptedExactly(t *testing.T) {
	other := "containerd://" + strings.Repeat("f", 64)
	for name, c := range map[string]struct {
		edit  func(n int32, pod *core.Pod)
		step  string
		calls int32
	}{
		"created-with-reason": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			createdNotStarted(&pod.Status.InitContainerStatuses[1])
			pod.Status.InitContainerStatuses[1].State.Waiting.Reason = "PodInitializing"
		}, "init-created", 1},
		"created-with-error-reason": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			createdNotStarted(&pod.Status.InitContainerStatuses[1])
			pod.Status.InitContainerStatuses[1].State.Waiting.Reason = "CreateContainerError"
		}, "init-created", 1},
		"created-with-message": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			createdNotStarted(&pod.Status.InitContainerStatuses[1])
			pod.Status.InitContainerStatuses[1].State.Waiting.Message = "x"
		}, "init-created", 1},
		"created-malformed-id": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			createdNotStarted(&pod.Status.InitContainerStatuses[1])
			pod.Status.InitContainerStatuses[1].ContainerID = "containerd://short"
		}, "init-created", 1},
		"created-before-install-done": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			runningInit(&pod.Status.InitContainerStatuses[0])
			createdNotStarted(&pod.Status.InitContainerStatuses[1])
		}, "init-created", 1},
		"created-after-install-failed": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			pod.Status.InitContainerStatuses[0].State.Terminated.ExitCode = 1
			createdNotStarted(&pod.Status.InitContainerStatuses[1])
		}, "toolchain-install", 1},
		"created-wrong-image": {func(_ int32, pod *core.Pod) {
			initializing(pod)
			createdNotStarted(&pod.Status.InitContainerStatuses[1])
			pod.Status.InitContainerStatuses[1].ImageID = "registry.example/go@sha256:" + strings.Repeat("f", 64)
		}, "init-image", 1},
		"created-replaced-when-started": {func(n int32, pod *core.Pod) {
			initializing(pod)
			if n == 1 {
				createdNotStarted(&pod.Status.InitContainerStatuses[1])
				pod.Status.InitContainerStatuses[1].ContainerID = other
				return
			}
			runningInit(&pod.Status.InitContainerStatuses[1])
		}, "init-unstable", 2},
		"running-then-created": {func(n int32, pod *core.Pod) {
			initializing(pod)
			if n == 1 {
				runningInit(&pod.Status.InitContainerStatuses[1])
				return
			}
			createdNotStarted(&pod.Status.InitContainerStatuses[1])
		}, "init-unstable", 2},
		"created-then-gone": {func(n int32, pod *core.Pod) {
			initializing(pod)
			if n == 1 {
				createdNotStarted(&pod.Status.InitContainerStatuses[1])
				return
			}
			waitingInit(&pod.Status.InitContainerStatuses[1], "PodInitializing")
		}, "init-unstable", 2},
		"main-created-during-copy": {func(_ int32, pod *core.Pod) {
			pod.Status.Phase = core.PodPending
			runningInit(&pod.Status.InitContainerStatuses[1])
			createdNotStarted(&pod.Status.ContainerStatuses[1])
		}, "containers-before-init", 1},
		"main-created-with-reason": {func(_ int32, pod *core.Pod) {
			createdNotStarted(&pod.Status.ContainerStatuses[1])
			pod.Status.ContainerStatuses[1].State.Waiting.Reason = "RunContainerError"
		}, "containers-created", 1},
		"main-running-then-created": {func(n int32, pod *core.Pod) {
			pod.Status.Phase = core.PodRunning
			if n == 1 {
				createdNotStarted(&pod.Status.ContainerStatuses[0])
				return
			}
			createdNotStarted(&pod.Status.ContainerStatuses[1])
		}, "containers-created", 2},
		"main-created-wrong-image": {func(_ int32, pod *core.Pod) {
			createdNotStarted(&pod.Status.ContainerStatuses[1])
			pod.Status.ContainerStatuses[1].ImageID = "fixture/other@sha256:" + strings.Repeat("f", 64)
		}, "containers", 1},
	} {
		t.Run(name, func(t *testing.T) {
			r, _, _, calls := toolchainResources(t, c.edit)
			session, err := r.WaitRunning(context.Background(), "22222222-2222-4222-8222-222222222222")
			if err == nil || session != nil || !errors.Is(err, ErrTransport) || calls.Load() != c.calls {
				t.Fatal("unsafe created state accepted or retried", err, calls.Load())
			}
			if ReadinessStep(err) != c.step || !KnownReadinessStep(c.step) {
				t.Fatal("readiness step", ReadinessStep(err), c.step)
			}
		})
	}
}
