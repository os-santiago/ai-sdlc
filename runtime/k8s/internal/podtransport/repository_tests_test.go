package podtransport

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repositorytests"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	core "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	protocol "k8s.io/apimachinery/pkg/util/remotecommand"
)

const testsPodUID = "44444444-4444-4444-8444-444444444444"

func testsFixturePod() (repositorytests.Pod, []byte) {
	b := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: strings.Repeat("a", 40), Files: []workspacebundle.File{{Path: "go.mod", Mode: 0o644, Data: []byte("module x\n")}}}
	raw, _ := workspacebundle.Encode(b)
	tree, _ := b.TreeDigest()
	return repositorytests.Pod{Namespace: VerifierNamespace,
		MailboxImage: "registry.example/mailbox@sha256:" + strings.Repeat("2", 64), RuntimeClass: executor.DefaultRuntimeClass, WorkItemID: "11111111-1111-4111-8111-111111111111",
		VerificationID: "verification-" + strings.Repeat("9", 64), Run: repositorytests.RunCandidate, SourceSHA: b.SourceSHA, TreeSHA256: tree,
		Verification: project.Verification{Image: "registry.example/go@sha256:" + strings.Repeat("3", 64), Command: []string{"go", "test", "./..."},
			TimeoutSeconds: 30, CPU: "1", Memory: "1Gi", EphemeralStorage: "1Gi", Network: "none", Result: project.Result{ExitCode: true},
			Repeat: 2, TestGlobs: []string{"**/*_test.go"}}}, raw
}

// testsCluster serves one pod whose status the test advances, the deny-all
// policy, the tests log and the two sidecar helpers.
type testsCluster struct {
	mu     sync.Mutex
	pod    core.Pod
	state  string // "pending", "running", "succeeded", "failed"
	log    string
	execs  [][]string
	reply  func(command []string) (string, bool)
	onExec func(command []string)
	// podFault, when set, answers the n-th pod GET (from 1): -1 drops the
	// connection, -2 serves the pod after 3.5 s, another non-zero value is
	// written as the status, 0 serves the pod.
	podFault func(n int) int
	podGets  int
}

func (c *testsCluster) status(name, image, id string, state core.ContainerState) core.ContainerStatus {
	s := core.ContainerStatus{Name: name, State: state, Image: image}
	if id != "" {
		s.ContainerID = "containerd://" + strings.Repeat(id, 64)
		s.ImageID = image
	}
	return s
}

func (c *testsCluster) current(t *TestsRun) core.Pod {
	c.mu.Lock()
	defer c.mu.Unlock()
	pod := *t.resources.pod.DeepCopy()
	applyDefaults(&pod.Spec)
	pod.APIVersion, pod.Kind, pod.UID = "v1", "Pod", testsPodUID
	pod.Spec.NodeName = "node"
	init, tests := pod.Spec.InitContainers, pod.Spec.Containers[0]
	completed := core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}}
	running := core.ContainerState{Running: &core.ContainerStateRunning{}}
	creating := core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "PodInitializing"}}
	install := c.status("install-runner", init[0].Image, "a", completed)
	install.State.Terminated.ContainerID = install.ContainerID
	switch c.state {
	case "initializing":
		pod.Status.Phase = core.PodPending
		copying := c.status("install-runner", init[0].Image, "a", running)
		pod.Status.InitContainerStatuses = []core.ContainerStatus{copying, c.status("transfer", init[1].Image, "", creating)}
		pod.Status.ContainerStatuses = []core.ContainerStatus{c.status("tests", tests.Image, "", creating)}
	case "stopping":
		pod.Status.Phase = core.PodRunning
		transfer := c.status("transfer", init[1].Image, "b", core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 137}})
		transfer.State.Terminated.ContainerID = transfer.ContainerID
		done := c.status("tests", tests.Image, "c", core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}})
		done.State.Terminated.ContainerID = done.ContainerID
		pod.Status.InitContainerStatuses = []core.ContainerStatus{install, transfer}
		pod.Status.ContainerStatuses = []core.ContainerStatus{done}
	case "pending":
		pod.Status.Phase = core.PodPending
		pod.Status.InitContainerStatuses = []core.ContainerStatus{install, c.status("transfer", init[1].Image, "b", running)}
		pod.Status.ContainerStatuses = []core.ContainerStatus{c.status("tests", tests.Image, "", creating)}
	case "transfer-created", "tests-created", "tests-created-error":
		// The kubelet's created-but-not-started status: waiting with the
		// container ID and no reason (the image ID may still be empty).
		created := core.ContainerState{Waiting: &core.ContainerStateWaiting{}}
		pod.Status.Phase = core.PodPending
		pod.Status.InitContainerStatuses = []core.ContainerStatus{install, c.status("transfer", init[1].Image, "b", running)}
		pod.Status.ContainerStatuses = []core.ContainerStatus{c.status("tests", tests.Image, "c", created)}
		if c.state == "transfer-created" {
			pod.Status.InitContainerStatuses[1] = c.status("transfer", init[1].Image, "b", created)
			pod.Status.InitContainerStatuses[1].ImageID = ""
			pod.Status.ContainerStatuses = []core.ContainerStatus{c.status("tests", tests.Image, "", creating)}
		}
		if c.state == "tests-created-error" {
			pod.Status.ContainerStatuses[0].State.Waiting.Reason = "CreateContainerError"
		}
	case "running":
		pod.Status.Phase = core.PodRunning
		pod.Status.InitContainerStatuses = []core.ContainerStatus{install, c.status("transfer", init[1].Image, "b", running)}
		pod.Status.ContainerStatuses = []core.ContainerStatus{c.status("tests", tests.Image, "c", running)}
	case "succeeded", "failed":
		pod.Status.Phase = core.PodSucceeded
		transfer := c.status("transfer", init[1].Image, "b", core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}})
		transfer.State.Terminated.ContainerID = transfer.ContainerID
		done := c.status("tests", tests.Image, "c", core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}})
		done.State.Terminated.ContainerID = done.ContainerID
		if c.state == "failed" {
			pod.Status.Phase = core.PodFailed
			done.State.Terminated.ExitCode = 1
		}
		pod.Status.InitContainerStatuses = []core.ContainerStatus{install, transfer}
		pod.Status.ContainerStatuses = []core.ContainerStatus{done}
	}
	return pod
}

func testsServer(t *testing.T, c *testsCluster) *TestsRun {
	t.Helper()
	p, source := testsFixturePod()
	var run *TestsRun
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/networkpolicies"):
			policy := networking.NetworkPolicy{ObjectMeta: meta.ObjectMeta{Name: "deny-all", Namespace: VerifierNamespace}, Spec: networking.NetworkPolicySpec{PolicyTypes: []networking.PolicyType{networking.PolicyTypeIngress, networking.PolicyTypeEgress}}}
			writeResource(w, 200, networking.NetworkPolicyList{Items: []networking.NetworkPolicy{policy}})
		case strings.HasSuffix(r.URL.Path, "/log"):
			if r.URL.Query().Get("container") != "tests" {
				t.Error("log of another container")
			}
			_, _ = w.Write([]byte(c.log))
		case strings.HasSuffix(r.URL.Path, "/exec"):
			if r.URL.Query().Get("container") != "transfer" {
				t.Error("helper outside the transfer sidecar")
			}
			command := r.URL.Query()["command"]
			c.mu.Lock()
			c.execs = append(c.execs, command)
			c.mu.Unlock()
			conn, err := (&websocket.Upgrader{Subprotocols: []string{protocol.StreamProtocolV5Name}}).Upgrade(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			if r.URL.Query().Get("stdin") == "true" {
				sourceInput(t, conn)
			}
			if c.onExec != nil {
				c.onExec(command)
			}
			out, ok := c.reply(command)
			workspaceReply(conn, []byte(out), ok)
		default:
			if c.podFault != nil {
				c.mu.Lock()
				c.podGets++
				n := c.podGets
				c.mu.Unlock()
				switch fault := c.podFault(n); {
				case fault == -2:
					time.Sleep(3500 * time.Millisecond) // slow but valid answer
				case fault == -1:
					if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
						_ = conn.Close()
					}
					return
				case fault != 0:
					w.WriteHeader(fault)
					return
				}
			}
			writeResource(w, 200, c.current(run))
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(Config{APIServer: server.URL, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), BearerToken: strings.Repeat("x", 32)})
	if err != nil {
		t.Fatal(err)
	}
	run, err = PlanTestsRun(client, p, RuntimeClassAdmission{}, source, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func honestReply(p repositorytests.Pod, source []byte) func([]string) (string, bool) {
	return func(command []string) (string, bool) {
		if command[1] == "import" {
			raw, _ := json.Marshal(map[string]any{"imported": true, "files": 1, "sourceSHA": p.SourceSHA, "bundleSHA256": workspacebundle.Digest(source)})
			return string(raw), true
		}
		return "", true
	}
}

func TestTestsRunImportsReadiesAndReadsOneResult(t *testing.T) {
	p, source := testsFixturePod()
	c := &testsCluster{state: "running", reply: honestReply(p, source), log: `{"exitCode":0}` + "\n" + `{"exitCode":0}` + "\n"}
	c.onExec = func(command []string) {
		if command[1] == "ready" {
			c.mu.Lock()
			c.state = "succeeded"
			c.mu.Unlock()
		}
	}
	run := testsServer(t, c)
	if _, _, err := run.ReadResult(context.Background(), testsPodUID); err == nil {
		t.Fatal("a result read before Prepare")
	}
	if err := run.Prepare(context.Background(), testsPodUID); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"/workspace-transfer", "import", "--pod-uid", testsPodUID, "--source-sha", p.SourceSHA, "--digest", workspacebundle.Digest(source)},
		{"/workspace-transfer", "ready", "--pod-uid", testsPodUID, "--source-sha", p.SourceSHA, "--tree-sha256", p.TreeSHA256},
	}
	if !reflect.DeepEqual(c.execs, want) {
		t.Fatal("helpers", c.execs)
	}
	got, raw, err := run.ReadResult(context.Background(), testsPodUID)
	if err != nil || len(got) != 2 || got[1].ExitCode != 0 || string(raw) != c.log {
		t.Fatal("result", got, string(raw), err)
	}
	if run.Prepare(context.Background(), testsPodUID) == nil {
		t.Fatal("a second Prepare ran helpers again")
	}
}

func TestTestsRunToleratesInitCopyAndSidecarStopOrder(t *testing.T) {
	p, source := testsFixturePod()
	c := &testsCluster{state: "initializing", reply: honestReply(p, source), log: strings.Repeat(`{"exitCode":0}`+"\n", 2)}
	run := testsServer(t, c)
	go func() {
		time.Sleep(400 * time.Millisecond)
		c.mu.Lock()
		c.state = "pending"
		c.mu.Unlock()
		time.Sleep(400 * time.Millisecond)
		c.mu.Lock()
		c.state = "running"
		c.mu.Unlock()
	}()
	if err := run.Prepare(context.Background(), testsPodUID); err != nil {
		t.Fatal("the init copy or a waiting sidecar failed Prepare", err)
	}
	c.mu.Lock()
	c.state = "stopping"
	c.mu.Unlock()
	go func() {
		time.Sleep(400 * time.Millisecond)
		c.mu.Lock()
		c.state = "succeeded"
		c.mu.Unlock()
	}()
	if _, _, err := run.ReadResult(context.Background(), testsPodUID); err != nil {
		t.Fatal("a sidecar stopped before the phase changed failed the run", err)
	}
}

func TestTestsRunRefusesAmbiguousResultsAndFailedPods(t *testing.T) {
	for name, log := range map[string]string{
		"one line":      `{"exitCode":0}` + "\n",
		"three lines":   strings.Repeat(`{"exitCode":0}`+"\n", 3),
		"no newline":    `{"exitCode":0}` + "\n" + `{"exitCode":0}`,
		"not a result":  `{"exitCode":0}` + "\nok\n",
		"carriage":      `{"exitCode":0}` + "\r\n" + `{"exitCode":0}` + "\n",
		"empty line":    `{"exitCode":0}` + "\n\n",
		"oversized":     strings.Repeat("x", 2*(repositorytests.MaxResultBytes+1)+1),
		"missing field": `{"exitCode":0}` + "\n" + `{"tests":{}}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			p, source := testsFixturePod()
			c := &testsCluster{state: "running", reply: honestReply(p, source), log: log}
			run := testsServer(t, c)
			if err := run.Prepare(context.Background(), testsPodUID); err != nil {
				t.Fatal(err)
			}
			c.mu.Lock()
			c.state = "succeeded"
			c.mu.Unlock()
			if _, _, err := run.ReadResult(context.Background(), testsPodUID); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	p, source := testsFixturePod()
	c := &testsCluster{state: "running", reply: honestReply(p, source), log: `{"exitCode":0}` + "\n"}
	run := testsServer(t, c)
	if err := run.Prepare(context.Background(), testsPodUID); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.state = "failed"
	c.mu.Unlock()
	if _, _, err := run.ReadResult(context.Background(), testsPodUID); err == nil {
		t.Fatal("a failed pod's log accepted")
	}
}

func TestTestsRunRefusesABadImportAndNeverReadiesIt(t *testing.T) {
	p, source := testsFixturePod()
	c := &testsCluster{state: "running", reply: func(command []string) (string, bool) {
		if command[1] == "import" {
			raw, _ := json.Marshal(map[string]any{"imported": true, "files": 2, "sourceSHA": p.SourceSHA, "bundleSHA256": workspacebundle.Digest(source)})
			return string(raw), true
		}
		return "", true
	}}
	run := testsServer(t, c)
	if run.Prepare(context.Background(), testsPodUID) == nil {
		t.Fatal("a wrong receipt accepted")
	}
	if len(c.execs) != 1 {
		t.Fatal("ready ran after a refused import", c.execs)
	}
}

func TestPlanTestsRunRefusesAnotherTreeOrNamespace(t *testing.T) {
	p, source := testsFixturePod()
	client, _ := NewClient(Config{APIServer: "https://127.0.0.1:1", CA: nil, BearerToken: strings.Repeat("x", 32)})
	if client == nil {
		client = &Client{}
	}
	other := p
	other.TreeSHA256 = strings.Repeat("f", 64)
	if _, err := PlanTestsRun(client, other, RuntimeClassAdmission{}, source, time.Now().Add(time.Minute)); err == nil {
		t.Fatal("a bundle for another tree accepted")
	}
	other = p
	other.Namespace = "default"
	if _, err := PlanTestsRun(client, other, RuntimeClassAdmission{}, source, time.Now().Add(time.Minute)); err == nil {
		t.Fatal("a namespace without the deny-all policy accepted")
	}
	if _, err := PlanTestsRun(client, p, RuntimeClassAdmission{}, source, time.Now().Add(-time.Second)); err == nil {
		t.Fatal("an expired run accepted")
	}
}

func TestTestsRunNamesTheStepThatStopped(t *testing.T) {
	p, source := testsFixturePod()
	honest := honestReply(p, source)
	step := func(t *testing.T, err error, want string) {
		t.Helper()
		if err == nil || !errors.Is(err, ErrTransport) || TestsStep(err) != want || !KnownTestsStep(want) {
			t.Fatal("step", TestsStep(err), "want", want, err)
		}
	}
	t.Run("observe-uid", func(t *testing.T) {
		run := testsServer(t, &testsCluster{state: "running", reply: honest})
		_, err := run.Status(context.Background(), "not-a-uid")
		step(t, err, "observe-uid")
	})
	t.Run("observe-phase", func(t *testing.T) {
		run := testsServer(t, &testsCluster{state: "failed", reply: honest})
		_, err := run.Status(context.Background(), testsPodUID)
		step(t, err, "observe-phase")
	})
	t.Run("import-before-state", func(t *testing.T) {
		run := testsServer(t, &testsCluster{state: "pending", reply: honest})
		step(t, run.Import(context.Background(), testsPodUID), "import-before-state")
		// Import runs at most once.
		step(t, run.Import(context.Background(), testsPodUID), "import-repeat")
	})
	t.Run("import-before-observe-phase", func(t *testing.T) {
		run := testsServer(t, &testsCluster{state: "failed", reply: honest})
		step(t, run.Import(context.Background(), testsPodUID), "import-before-observe-phase")
	})
	t.Run("import-receipt", func(t *testing.T) {
		run := testsServer(t, &testsCluster{state: "running", reply: func(command []string) (string, bool) {
			if command[1] == "import" {
				return `{"imported":false}`, true
			}
			return "", true
		}})
		step(t, run.Import(context.Background(), testsPodUID), "import-receipt")
	})
	t.Run("import-after-state", func(t *testing.T) {
		c := &testsCluster{state: "running", reply: honest}
		c.onExec = func(command []string) {
			c.mu.Lock()
			c.state = "pending"
			c.mu.Unlock()
		}
		run := testsServer(t, c)
		step(t, run.Import(context.Background(), testsPodUID), "import-after-state")
	})
	// Output from ready exceeds its zero output bound: the exchange fails.
	t.Run("ready-transfer-request", func(t *testing.T) {
		run := testsServer(t, &testsCluster{state: "running", reply: func(command []string) (string, bool) {
			if command[1] == "ready" {
				return "unexpected", true
			}
			return honest(command)
		}})
		step(t, run.Import(context.Background(), testsPodUID), "ready-transfer-request")
	})
	t.Run("ready-after-observe-phase", func(t *testing.T) {
		c := &testsCluster{state: "running", reply: honest}
		c.onExec = func(command []string) {
			if command[1] == "ready" {
				c.mu.Lock()
				c.state = "failed"
				c.mu.Unlock()
			}
		}
		run := testsServer(t, c)
		step(t, run.Import(context.Background(), testsPodUID), "ready-after-observe-phase")
	})
}

func TestTestsStepsAreBounded(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for want, err := range map[string]error{
		"import-transfer-permission":     testsFailure(exchangeStep("import-transfer", context.Background(), ErrPermission), ErrPermission),
		"import-transfer-timeout":        testsFailure(exchangeStep("import-transfer", canceled, ErrTransport), ErrTransport),
		"ready-transfer-request":         testsFailure(exchangeStep("ready-transfer", context.Background(), ErrTransport), ErrTransport),
		"import-before-observe-request":  importFailure("import-before", testsFailure(requestStep("observe", ErrTransport), ErrTransport)),
		"ready-after-observe-permission": importFailure("ready-after", testsFailure(requestStep("observe", ErrPermission), ErrPermission)),
	} {
		if TestsStep(err) != want {
			t.Fatal("step", TestsStep(err), want)
		}
	}
	// Import still stops with ErrTransport, as it always has.
	if err := importFailure("ready-after", testsFailure("observe-permission", ErrPermission)); !errors.Is(err, ErrTransport) || errors.Is(err, ErrPermission) {
		t.Fatal("import stop changed", err)
	}
	if err := testsFailure("observe-permission", ErrPermission); !errors.Is(err, ErrPermission) {
		t.Fatal("observation cause lost", err)
	}
	for _, step := range []string{"", "observe", "observe-other", "import-before-observe-uid-x", "ready-transfer-exit", "candidate-observe-request", "import-transfer"} {
		if KnownTestsStep(step) || TestsStep(testsFailure(step, ErrTransport)) != "" {
			t.Fatal("unbounded step accepted", step)
		}
	}
}

func TestTestsRunAcceptsTheKubeletCreatedState(t *testing.T) {
	p, source := testsFixturePod()
	c := &testsCluster{state: "transfer-created", reply: honestReply(p, source), log: strings.Repeat(`{"exitCode":0}`+"\n", 2)}
	run := testsServer(t, c)
	for _, state := range []string{"transfer-created", "tests-created"} {
		c.mu.Lock()
		c.state = state
		c.mu.Unlock()
		if status, err := run.Status(context.Background(), testsPodUID); err != nil || status.Ready || status.Succeeded {
			t.Fatal("created container refused or taken as ready", state, err)
		}
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		c.mu.Lock()
		c.state = "running"
		c.mu.Unlock()
	}()
	if err := run.Prepare(context.Background(), testsPodUID); err != nil {
		t.Fatal("a created tests container failed Prepare", err)
	}
	// A waiting container with an ID and a reason is a failed start.
	c2 := &testsCluster{state: "tests-created-error", reply: honestReply(p, source)}
	run2 := testsServer(t, c2)
	if _, err := run2.Status(context.Background(), testsPodUID); !errors.Is(err, ErrTransport) || TestsStep(err) != "observe-container" {
		t.Fatal("failed start accepted", err)
	}
}

// One dropped connection or 429/5xx answer while observing the pod is
// repeated within the observation's bound (evaluation battery 28 stopped a
// verification at baseline-observe-request); a refusal is not, and a
// persistent failure is still named observe-request.
func TestTestsRunObservationRetriesTransientFailures(t *testing.T) {
	p, source := testsFixturePod()
	honest := honestReply(p, source)
	for _, which := range []string{"dropped", "503", "429", "slow", "forbidden", "always-dropped"} {
		t.Run(which, func(t *testing.T) {
			c := &testsCluster{state: "running", reply: honest, podFault: func(n int) int {
				switch {
				case which == "slow" && n == 1:
					return -2
				case which == "always-dropped", which == "dropped" && n == 1:
					return -1
				case which == "503" && n == 1:
					return 503
				case which == "429" && n == 1:
					return 429
				case which == "forbidden":
					return 403
				}
				return 0
			}}
			run := testsServer(t, c)
			started := time.Now()
			status, err := run.Status(context.Background(), testsPodUID)
			if time.Since(started) > observeRetryWindow+6*time.Second {
				t.Fatal("observation exceeded its bound")
			}
			switch which {
			case "forbidden":
				if TestsStep(err) != "observe-permission" || c.podGets != 1 {
					t.Fatalf("refusal retried or misnamed: %q gets=%d", TestsStep(err), c.podGets)
				}
			case "always-dropped":
				if TestsStep(err) != "observe-request" || c.podGets < 2 {
					t.Fatalf("persistent failure %q gets=%d", TestsStep(err), c.podGets)
				}
			case "slow":
				// A slow answer within request's own 5 s bound is not cut
				// short by the retry window.
				if err != nil || !status.Ready || c.podGets != 1 {
					t.Fatalf("slow valid answer refused: %v gets=%d", err, c.podGets)
				}
			default:
				if err != nil || !status.Ready {
					t.Fatalf("transient failure not retried: %v", err)
				}
			}
		})
	}
}
