package podtransport

import (
	"context"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"io"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// browserFixtureTrace captures only finite public status from existing Pod GETs.
// It performs no requests and restores every response byte for the real verifier.
type browserFixtureTrace struct {
	next http.RoundTripper
	mu   sync.Mutex
	last map[string]any
}

func (d *browserFixtureTrace) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := d.next.RoundTrip(req)
	prefix := "/api/v1/namespaces/" + browsercheck.Namespace + "/pods/"
	if err != nil || response.Body == nil || req.Method != "GET" || !strings.HasPrefix(req.URL.Path, prefix) || strings.Contains(strings.TrimPrefix(req.URL.Path, prefix), "/") {
		return response, err
	}
	response.Body = &browserFixtureBody{ReadCloser: response.Body, observe: d.observe}
	return response, err
}
func (d *browserFixtureTrace) observe(raw []byte) {
	var pod core.Pod
	if json.Unmarshal(raw, &pod) != nil || pod.Kind != "Pod" {
		return
	}
	status := map[string]any{"phase": browserFixtureEnum(string(pod.Status.Phase), "Pending", "Running", "Succeeded", "Failed", "Unknown"), "containers": len(pod.Status.ContainerStatuses)}
	if len(pod.Status.ContainerStatuses) == 1 {
		cs := pod.Status.ContainerStatuses[0]
		status["restartCount"] = cs.RestartCount
		status["runtimeDigest"] = browserFixtureDigest(cs.ImageID)
		if cs.State.Terminated != nil {
			status["state"] = "terminated"
			status["exitCode"] = cs.State.Terminated.ExitCode
			status["reason"] = browserFixtureEnum(cs.State.Terminated.Reason, "Completed", "Error", "OOMKilled", "StartError", "ContainerCannotRun", "DeadlineExceeded")
		}
		if cs.State.Waiting != nil {
			status["state"] = "waiting"
			status["reason"] = browserFixtureEnum(cs.State.Waiting.Reason, "ContainerCreating", "PodInitializing", "ErrImagePull", "ImagePullBackOff", "CreateContainerConfigError", "CreateContainerError", "CrashLoopBackOff")
		}
		if cs.State.Running != nil {
			status["state"] = "running"
		}
	}
	d.mu.Lock()
	d.last = status
	d.mu.Unlock()
}

type browserFixtureBody struct {
	io.ReadCloser
	raw                 []byte
	complete, oversized bool
	observe             func([]byte)
}

func (b *browserFixtureBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if !b.oversized {
		if len(b.raw)+n > 128*1024 {
			b.oversized = true
			b.raw = nil
		} else {
			b.raw = append(b.raw, p[:n]...)
		}
	}
	if err == io.EOF {
		b.complete = true
	}
	return n, err
}
func (b *browserFixtureBody) Close() error {
	err := b.ReadCloser.Close()
	if b.complete && !b.oversized {
		b.observe(b.raw)
	}
	return err
}
func browserFixtureEnum(value string, allowed ...string) string {
	for _, x := range allowed {
		if x == value {
			return x
		}
	}
	return "other"
}
func browserFixtureDigest(value string) string {
	_, digest, ok := strings.Cut(value, "sha256:")
	if !ok || len(digest) != 64 {
		return "unavailable"
	}
	for _, c := range digest {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "unavailable"
		}
	}
	return "sha256:" + digest
}

func TestRealBrowserDeploymentTransport(t *testing.T) {
	path := os.Getenv("AI_SDLC_BROWSER_TRANSPORT_FIXTURE")
	if path == "" {
		t.Skip("owned browser transport fixture required")
	}
	raw, e := os.ReadFile(path)
	var input struct {
		Client      Config
		Target      releasing.Target
		Profile     browsercheck.Profile
		Report      string
		Accept      bool
		CleanupOnly bool
	}
	if e != nil {
		t.Fatal("fixture input unreadable")
	}
	if len(raw) > 256*1024 || authority.DecodeStrict(raw, &input) != nil || input.Report == "" {
		t.Fatal("invalid fixture input")
	}
	started := time.Now()
	stage := "client"
	var diagnosticGate *browserGateFixture
	var trace *browserFixtureTrace
	defer func() {
		if !t.Failed() {
			return
		}
		report := map[string]any{"status": "failed", "stage": stage, "elapsedMilliseconds": time.Since(started).Milliseconds(), "expectedAccept": input.Accept, "cleanupOnly": input.CleanupOnly, "autonomousE2E": false, "scope": "real-kubernetes-browser-transport", "expectedRuntimeDigest": input.Profile.RuntimeDigest}
		if diagnosticGate != nil {
			report["createCount"] = diagnosticGate.sent
			report["originalPodAckCount"] = diagnosticGate.ack
			report["recordCount"] = diagnosticGate.record
		}
		if trace != nil {
			trace.mu.Lock()
			report["lastObservedBrowserPod"] = trace.last
			trace.mu.Unlock()
		}
		raw, err := json.MarshalIndent(report, "", "  ")
		if err != nil || os.WriteFile(input.Report, append(raw, '\n'), 0644) != nil {
			t.Error("failed public diagnostic report")
		}
	}()
	client, e := NewClient(input.Client)
	if e != nil {
		t.Fatal(e)
	}
	trace = &browserFixtureTrace{next: client.http.Transport}
	client.http.Transport = trace
	stage = "original-resources"
	reader := &ConsoleDeployment{client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, e = reader.request(ctx, "GET", consolePath, nil)
	var d apps.Deployment
	if e != nil || authority.DecodeStrict(raw, &d) != nil {
		t.Fatal("original fixture deployment")
	}
	input.Target.DeploymentSpecSHA256, e = ConsoleSpecDigest(d.Spec)
	if e != nil {
		t.Fatal(e)
	}
	raw, e = reader.request(ctx, "GET", "/api/v1/namespaces/"+consoleNS+"/services/"+consoleName, nil)
	var s core.Service
	if e != nil || authority.DecodeStrict(raw, &s) != nil {
		t.Fatal("original fixture service")
	}
	input.Target.ServiceSpecSHA256, e = ConsoleServiceDigest(s.Spec)
	if e != nil {
		t.Fatal(e)
	}
	adapter, e := NewConsoleDeployment(client, input.Target)
	if e != nil {
		t.Fatal(e)
	}
	stage = "observe"
	snapshot, e := adapter.Observe(ctx)
	if e != nil {
		t.Fatal("original snapshot", e)
	}
	// Match the runtime: wait for the original Service/EndpointSlice route,
	// not merely Deployment Ready, before starting its one smoke attempt.
	stage = "route-convergence"
	if _, e = adapter.WaitReady(ctx, snapshot); e != nil {
		t.Fatal("original fixture route convergence", e)
	}
	stage = "http-health"
	health, e := adapter.VerifyHealth(ctx, snapshot)
	if e != nil {
		t.Fatal("real HTTP/TLS before browser", e)
	}
	now := time.Now()
	until, _ := ctx.Deadline()
	raw, _ = json.Marshal(snapshot)
	request := browsercheck.Request{Version: browsercheck.RequestVersion, RequestID: "browser-" + authority.Digest([]byte(input.Report+now.String())), TargetSHA256: input.Target.Digest(), SnapshotSHA256: authority.Digest(raw), ImageDigest: input.Target.BaselineRuntimeDigest, IssuedAt: now.UnixMilli(), ExpiresAt: until.UnixMilli(), Expectation: input.Profile.BaselineExpectation}
	if input.CleanupOnly {
		request.ExpiresAt = time.Now().Add(3 * time.Second).UnixMilli()
	}
	b, e := NewBrowserResources(adapter, input.Profile, request, health.Rollout)
	if e != nil {
		t.Fatal(e)
	}
	stage = "scope"
	installation := BrowserInstallation{Target: input.Target, Profile: input.Profile}
	for path, value := range map[string]any{
		"/api/v1/namespaces/kube-system":                                                                   &installation.Cluster,
		"/api/v1/namespaces/" + browsercheck.Namespace:                                                     &installation.Namespace,
		"/api/v1/nodes/" + input.Profile.NodeName:                                                          &installation.Node,
		"/apis/node.k8s.io/v1/runtimeclasses/" + browsercheck.RuntimeClass:                                 &installation.RuntimeClass,
		"/api/v1/namespaces/" + browsercheck.Namespace + "/configmaps/" + browserCA:                        &installation.CA,
		"/apis/networking.k8s.io/v1/namespaces/" + browsercheck.Namespace + "/networkpolicies":             &installation.Policies,
		"/apis/networking.k8s.io/v1/namespaces/ai-sdlc-production/networkpolicies/ai-sdlc-browser-console": &installation.ConsoleIngress,
	} {
		if b.read(ctx, path, value) != nil {
			t.Fatal("fixture public installation observation")
		}
	}
	if installation.Validate(time.Now()) != nil {
		t.Fatal("offline compiler rejected real scope")
	}
	originalUID := installation.Node.UID
	installation.Node.UID = "99999999-1111-1111-1111-111111111111"
	if installation.Validate(time.Now()) == nil {
		t.Fatal("offline compiler adopted foreign node")
	}
	installation.Node.UID = originalUID
	attempt, cancelAttempt := context.WithCancel(ctx)
	defer cancelAttempt()
	gate := &browserGateFixture{until: until, parent: attempt}
	if input.CleanupOnly {
		gate.onACK = cancelAttempt
	}
	diagnosticGate = gate
	stage = "browser-verify"
	_, evidence, err := b.Verify(attempt, snapshot, health.Rollout, gate)
	if input.Accept {
		if err != nil || evidence.Validate(input.Profile, time.Now()) != nil || gate.sent != 1 || gate.ack != 1 || gate.record != 1 {
			t.Fatalf("real browser lifecycle: err=%v gate=%+v", err, gate)
		}
	} else if err == nil || gate.sent != 1 || gate.ack != 1 || gate.record != 0 {
		t.Fatalf("real browser negative: err=%v gate=%+v", err, gate)
	}
	if input.CleanupOnly {
		stage = "fresh-cleanup"
		if gate.originalUID == "" {
			t.Fatal("cleanup fixture lacks original acknowledged Pod")
		}
		var original core.Pod
		if b.read(ctx, "/api/v1/namespaces/"+browsercheck.Namespace+"/pods/"+b.pod.Name, &original) != nil || string(original.UID) != gate.originalUID {
			t.Fatal("cancelled claim unexpectedly deleted or replaced original Pod")
		}
		time.Sleep(time.Until(time.UnixMilli(request.ExpiresAt).Add(50 * time.Millisecond)))
		cleaner, e := NewBrowserCleanupClient(client)
		if e != nil {
			t.Fatal(e)
		}
		fresh, stop := context.WithTimeout(ctx, 10*time.Second)
		defer stop()
		containment := &browserContainmentFixture{}
		if cleaner.CleanupBrowser(fresh, input.Profile, request, health.Rollout, gate.originalUID, containment) != nil || containment.records != 1 || gate.record != 0 {
			t.Fatal("fresh containment failed or invented late health")
		}
	}
	stage = "final-absence"
	var remaining core.PodList
	if b.read(ctx, "/api/v1/namespaces/"+browsercheck.Namespace+"/pods", &remaining) != nil || len(remaining.Items) != 0 {
		t.Fatal("original browser Pod not removed")
	}
	stage = "final-application"
	observed, e := adapter.Observe(ctx)
	observed.ObservedAt = snapshot.ObservedAt
	if e != nil || observed != snapshot {
		t.Fatal("fixture changed original application")
	}
	report := map[string]any{"cleanupOnly": input.CleanupOnly, "lateHealthRejected": input.CleanupOnly, "scope": "real-kubernetes-browser-transport", "autonomousE2E": false, "syntheticBusinessAPI": true, "expectedAccept": input.Accept, "status": "passed", "createCount": gate.sent, "originalPodAckCount": gate.ack, "recordCount": gate.record, "originalPodDeleted": true, "originalDeploymentUnchanged": true, "runtimeDigest": input.Profile.RuntimeDigest, "desktopMobileValidated": input.Accept, "normalTLS": true, "remainingSmokeMilliseconds": time.Until(until).Milliseconds()}
	if input.Accept {
		report["browserEvidenceSHA256"] = evidence.Digest()
	}
	raw, e = json.MarshalIndent(report, "", "  ")
	if e != nil || os.WriteFile(input.Report, append(raw, '\n'), 0644) != nil {
		t.Fatal("fixture report")
	}
}

func TestBrowserFixtureTracePreservesResponseAndBoundsPublicStatus(t *testing.T) {
	for _, large := range []bool{false, true} {
		d := &browserFixtureTrace{}
		message := "private arbitrary message"
		if large {
			message = strings.Repeat("x", 128*1024)
		}
		raw, _ := json.Marshal(core.Pod{TypeMeta: metav1.TypeMeta{Kind: "Pod"}, Status: core.PodStatus{Phase: core.PodFailed, Message: message, ContainerStatuses: []core.ContainerStatus{{ImageID: "registry/name@sha256:" + strings.Repeat("a", 64), State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 3, Reason: "private reason", Message: message}}}}}})
		body := &browserFixtureBody{ReadCloser: io.NopCloser(strings.NewReader(string(raw))), observe: d.observe}
		got, err := io.ReadAll(body)
		closeErr := body.Close()
		if err != nil || closeErr != nil || string(got) != string(raw) {
			t.Fatal("diagnostics changed response")
		}
		if large {
			if d.last != nil {
				t.Fatal("oversized diagnostic accepted")
			}
			continue
		}
		public, _ := json.Marshal(d.last)
		if strings.Contains(string(public), "private") || d.last["reason"] != "other" || d.last["exitCode"] != int32(3) {
			t.Fatal("unbounded status or missing finite evidence")
		}
	}
}
