package podtransport

import (
	"context"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	"os"
	"testing"
	"time"
)

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
	if e != nil || len(raw) > 256*1024 || authority.DecodeStrict(raw, &input) != nil || input.Report == "" {
		t.Fatal("invalid fixture input")
	}
	client, e := NewClient(input.Client)
	if e != nil {
		t.Fatal(e)
	}
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
	snapshot, e := adapter.Observe(ctx)
	if e != nil {
		t.Fatal("original snapshot", e)
	}
	// Match the runtime: wait for the original Service/EndpointSlice route,
	// not merely Deployment Ready, before starting its one smoke attempt.
	if _, e = adapter.WaitReady(ctx, snapshot); e != nil {
		t.Fatal("original fixture route convergence", e)
	}
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
	_, evidence, err := b.Verify(attempt, snapshot, health.Rollout, gate)
	if input.Accept {
		if err != nil || evidence.Validate(input.Profile, time.Now()) != nil || gate.sent != 1 || gate.ack != 1 || gate.record != 1 {
			t.Fatalf("real browser lifecycle: err=%v gate=%+v", err, gate)
		}
	} else if err == nil || gate.sent != 1 || gate.ack != 1 || gate.record != 0 {
		t.Fatalf("real browser negative: err=%v gate=%+v", err, gate)
	}
	if input.CleanupOnly {
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
	var remaining core.PodList
	if b.read(ctx, "/api/v1/namespaces/"+browsercheck.Namespace+"/pods", &remaining) != nil || len(remaining.Items) != 0 {
		t.Fatal("original browser Pod not removed")
	}
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
	if e != nil || os.WriteFile(input.Report, append(raw, '\n'), 0600) != nil {
		t.Fatal("fixture report")
	}
}
