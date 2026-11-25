package execution_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
)

// pollUnavailableGateway polls once with the model gateway unavailable or,
// with sandboxMissing, with a ready gateway but no sandbox RuntimeClass. Either
// way the poll stops before claiming work, after the expiry sweeps.
func pollUnavailableGateway(t *testing.T, f execution.RepairFixtureForTest, sandboxMissing bool) {
	t.Helper()
	t.Setenv("AI_SDLC_RUNTIME_MODE", "candidate")
	root := t.TempDir()
	write := func(name string, b []byte) {
		t.Helper()
		if e := os.WriteFile(filepath.Join(root, name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	policy := f.Policy
	policy.Grants = slices.DeleteFunc(slices.Clone(policy.Grants), func(g authority.Grant) bool {
		return g.Operation != "admit-issue" && g.Operation != "plan-admission" && g.Operation != "execute-work"
	})
	for name, value := range map[string]any{"policy": policy, "identity": f.Identities["worker"], "intake": f.Config} {
		raw, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		write(name, raw)
	}
	// These installation inputs are only used for containment and the gateway
	// check. There is no attempt to authorize the expired fixture command anew.
	config := f.Runtime
	config.Profile.Model = "codex-cli"
	config.Placement.Namespace = "ai-sdlc-executors"
	raw, _ := json.Marshal(config)
	write("runtime", raw)
	token := []byte(strings.Repeat("x", 48))
	write("token", token)
	write("model", token)
	var apiCalls, classReads, gatewayCalls atomic.Int32
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/apis/node.k8s.io/v1/runtimeclasses/ai-sdlc-offline" {
			classReads.Add(1)
			w.WriteHeader(404)
			return
		}
		apiCalls.Add(1)
		w.WriteHeader(503)
	}))
	defer api.Close()
	write("ca", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw}))
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gatewayCalls.Add(1)
		if sandboxMissing && r.URL.Path == "/readyz" {
			w.Write([]byte(`{"ready":true}`))
			return
		}
		w.WriteHeader(503)
	}))
	defer gateway.Close()
	runtime := execution.Runtime{Component: &bootstrap.Component{Name: "worker", DB: f.Worker, PolicyPath: filepath.Join(root, "policy"), IdentityPath: filepath.Join(root, "identity")}, ConfigPath: filepath.Join(root, "runtime"), IntakePath: filepath.Join(root, "intake"), TokenPath: filepath.Join(root, "token"), CAPath: filepath.Join(root, "ca"), GatewayTokenPath: filepath.Join(root, "model"), APIServer: api.URL, ProviderEndpoint: gateway.URL + "/v1/chat/completions"}
	_, e := runtime.PollOnce(context.Background())
	if e == nil {
		t.Fatal("unavailable gateway or sandbox accepted")
	}
	if gatewayCalls.Load() == 0 || apiCalls.Load() != 0 {
		t.Fatal("fixture did not reach unavailable gateway without Kubernetes effects", gatewayCalls.Load(), apiCalls.Load())
	}
	if sandboxMissing && (classReads.Load() != 1 || execution.DiagnosticStage(e) != "sandbox-runtime-class-not-found") {
		t.Fatal("missing sandbox class did not stop the claim", classReads.Load(), execution.DiagnosticStage(e))
	}
	if !sandboxMissing && classReads.Load() != 0 {
		t.Fatal("sandbox class read before the gateway check")
	}
}
