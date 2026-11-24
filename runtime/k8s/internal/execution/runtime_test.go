package execution

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func runtimeFixture() RuntimeConfig {
	return RuntimeConfig{Version: "ai-sdlc.candidate-runtime/v1", ADev: "reviewed doctrine", Profile: executor.Profile{Version: executor.ProfileVersion, Model: "codex-cli", MaxSteps: 4, MaxSeconds: 60, MaxTokens: 1024, ADevSHA256: workspacebundle.Digest([]byte("reviewed doctrine"))}, Placement: Placement{Namespace: "ai-sdlc-executors", WorkerImage: "example/worker@sha256:" + strings.Repeat("a", 64), MailboxImage: "example/mailbox@sha256:" + strings.Repeat("b", 64)}, Model: ModelPolicy{Limits: store.ModelLimits{Calls: 4, InputBytes: 131072, OutputBytes: 1048576}, ResponseLimit: 262144}}
}
func TestRuntimeRejectsUnsafeInstallationInputs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.json")
	for name, mutate := range map[string]func(*RuntimeConfig){
		"valid": func(*RuntimeConfig) {}, "mutable image": func(c *RuntimeConfig) { c.Placement.WorkerImage = "image:latest" }, "control namespace": func(c *RuntimeConfig) { c.Placement.Namespace = "ai-sdlc-runtime" }, "unsupported model": func(c *RuntimeConfig) { c.Profile.Model = "other" }, "doctrine substitution": func(c *RuntimeConfig) { c.ADev = "changed" }, "unbounded calls": func(c *RuntimeConfig) { c.Model.Limits.Calls = 1000 }, "invalid runtime class": func(c *RuntimeConfig) { c.Placement.RuntimeClass = "Kata/../runc" },
	} {
		t.Run(name, func(t *testing.T) {
			c := runtimeFixture()
			mutate(&c)
			raw, _ := json.Marshal(c)
			if e := os.WriteFile(path, raw, 0600); e != nil {
				t.Fatal(e)
			}
			_, _, e := LoadRuntime(path)
			if (e == nil) != (name == "valid") {
				t.Fatalf("configuration decision %v", e)
			}
		})
	}
	raw, _ := json.Marshal(runtimeFixture())
	raw = append(raw[:len(raw)-1], []byte(`,"version":"ai-sdlc.candidate-runtime/v1"}`)...)
	_ = os.WriteFile(path, raw, 0600)
	if _, _, e := LoadRuntime(path); e == nil {
		t.Fatal("duplicate configuration accepted")
	}
}
func TestRuntimeCarriesInstalledSandboxRuntimeClass(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.json")
	// The default document is unchanged: no runtime class field is written,
	// and the executor still runs in the installer's default class.
	c := runtimeFixture()
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "RuntimeClass") {
		t.Fatal("default placement changed its installed bytes")
	}
	if e := os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	loaded, _, e := LoadRuntime(path)
	if e != nil || loaded.Placement.SandboxRuntimeClass() != executor.DefaultRuntimeClass {
		t.Fatal("default sandbox runtime class not used", e)
	}
	c.Placement.RuntimeClass = "kata-mshv-vm-isolation"
	raw, _ = json.Marshal(c)
	if e := os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	loaded, _, e = LoadRuntime(path)
	if e != nil || loaded.Placement.SandboxRuntimeClass() != "kata-mshv-vm-isolation" {
		t.Fatal("configured sandbox runtime class not loaded", e)
	}
}
func TestCandidatePrivilegesAreLeastPrivilegeAndRevocable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	passwords := map[string]string{}
	for role := range f.Material.Identities {
		passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
	}
	if e := f.Admin.ProvisionRolesForMode(ctx, passwords, "candidate"); e != nil {
		t.Fatal(e)
	}
	if _, e := f.accept(f.envelope(t, f.command)); e != nil {
		t.Fatal(e)
	}
	if _, e := f.db.ReadExecution(ctx, f.command.ID()); e != nil {
		t.Fatal(e)
	}
	for _, sql := range []string{
		`UPDATE execution_requests SET payload='tampered' WHERE false`,
		`UPDATE intake_observations SET observed_at=clock_timestamp() WHERE false`,
		`UPDATE model_calls SET expires_at=clock_timestamp() WHERE false`,
		`SELECT * FROM managed_component_roles`,
		`INSERT INTO release_gates(repository,pull_request,state,correlation_id) VALUES('other/project',1,'verified','forged')`,
	} {
		if _, e := f.db.Pool.Exec(sql); e == nil {
			t.Fatalf("worker obtained forbidden privilege: %s", sql)
		}
	}
	if e := f.Admin.ProvisionRolesForMode(ctx, passwords, "shadow"); e != nil {
		t.Fatal(e)
	}
	if _, e := f.db.ReadExecution(ctx, f.command.ID()); e == nil {
		t.Fatal("downgrade retained execution access")
	}
	if _, e := f.db.Pool.Exec(`UPDATE work_items SET status='running' WHERE false`); e == nil {
		t.Fatal("downgrade retained column grant")
	}
}
func TestExecutionQueueDoesNotAdoptOrStopAnActiveClaim(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, e := f.accept(f.envelope(t, f.command)); e != nil {
		t.Fatal(e)
	}
	id, e := f.db.NextExecution(ctx)
	if e != nil || id != f.command.ID() {
		t.Fatal("queued identifier missing", e)
	}
	claim, e := Claim(ctx, f.db, f.readyPolicy, id)
	if e != nil {
		t.Fatal(e)
	}
	if id, e = f.db.NextExecution(ctx); e != nil || id != "" {
		t.Fatal("claimed request selected", e)
	}
	if e = f.db.RejectQueuedExecution(ctx, claim.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = f.db.ReadClaimedExecution(ctx, claim); e != nil {
		t.Fatal("queue rejection stopped an active owner")
	}
}
func TestExecutionQueueExpiresWithoutClaimOrRetry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, e := f.accept(f.envelope(t, f.command)); e != nil {
		t.Fatal(e)
	}
	if _, e := f.Admin.Pool.Exec(`UPDATE execution_requests SET expires_at=clock_timestamp()-interval '1 second'`); e != nil {
		t.Fatal(e)
	}
	if e := f.db.ExpireQueuedExecutions(ctx); e != nil {
		t.Fatal(e)
	}
	if id, e := f.db.NextExecution(ctx); e != nil || id != "" {
		t.Fatal("expired work selected")
	}
	if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='stopped' AND stop_reason='expired' AND claim_token IS NULL`) != 1 {
		t.Fatal("expiry did not preserve no-claim evidence")
	}
}

func TestCheckSandboxReadsOnlyTheInstalledRuntimeClass(t *testing.T) {
	var requested []string
	present := "kata"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.Method+" "+r.URL.Path)
		if r.Method != http.MethodGet || r.URL.Path != "/apis/node.k8s.io/v1/runtimeclasses/"+present {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{"kind":"RuntimeClass","apiVersion":"node.k8s.io/v1","metadata":{"name":"` + present + `"},"handler":"kata"}`))
	}))
	defer server.Close()
	root := t.TempDir()
	write := func(name string, raw []byte) string {
		path := filepath.Join(root, name)
		if e := os.WriteFile(path, raw, 0600); e != nil {
			t.Fatal(e)
		}
		return path
	}
	c := runtimeFixture()
	c.Placement.RuntimeClass = "kata"
	raw, _ := json.Marshal(c)
	r := Runtime{ConfigPath: write("runtime.json", raw), TokenPath: write("token", []byte(strings.Repeat("x", 32))),
		CAPath: write("ca", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})), APIServer: server.URL}
	if e := r.CheckSandbox(context.Background()); e != nil {
		t.Fatal(e)
	}
	// The default configuration names ai-sdlc-offline, which is absent here.
	raw, _ = json.Marshal(runtimeFixture())
	write("runtime.json", raw)
	if e := r.CheckSandbox(context.Background()); e == nil {
		t.Fatal("missing sandbox runtime class accepted")
	}
	if len(requested) != 2 || requested[0] != "GET /apis/node.k8s.io/v1/runtimeclasses/kata" || requested[1] != "GET /apis/node.k8s.io/v1/runtimeclasses/ai-sdlc-offline" {
		t.Fatal("unexpected API requests", requested)
	}
}
