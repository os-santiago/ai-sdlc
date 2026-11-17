package execution_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repairflow"
)

// The predecessor issue/source/behavior/review are synthetic journal fixtures.
// The child uses the real signed flow, PostgreSQL roles, SCC, gVisor and helpers.
// This proves the repair controller boundary, never autonomous application E2E.
func TestRealConnectedRepairControllerFixture(t *testing.T) {
	path := os.Getenv("AI_SDLC_CANDIDATE_FIXTURE")
	if path == "" {
		t.Skip("explicit isolated repair Kubernetes fixture required")
	}
	var in struct {
		Config               podtransport.Config
		Runtime              execution.RuntimeConfig
		ReportFile, Scenario string
	}
	raw, e := os.ReadFile(path)
	if e != nil || len(raw) > 256*1024 || json.Unmarshal(raw, &in) != nil || in.Scenario != "repair" {
		t.Fatal("private repair fixture input")
	}
	f := execution.NewKubeRepairFixtureForTest(t, in.Runtime.Placement, []byte(in.Runtime.ADev))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	var original string
	if e = f.Admin.Pool.QueryRow(`SELECT row_to_json(b)::text FROM model_budgets b`).Scan(&original); e != nil {
		t.Fatal(e)
	}
	if sent, e := repairflow.Handoff(ctx, f.Verifier, repairLoad(f, "verifier")); e != nil || !sent {
		t.Fatal("repair handoff", sent, e)
	}
	if _, e = relayRepair(t, f, "repair-candidate", repairLoad(f, "orchestrator")); e != nil {
		t.Fatal("repair inbox", e)
	}
	if _, e = relayRepair(t, f, "execute-work", nil); e != nil {
		t.Fatal("child inbox", e)
	}
	var child string
	if e = f.Admin.Pool.QueryRow(`SELECT child_request_id FROM execution_repairs`).Scan(&child); e != nil {
		t.Fatal(e)
	}
	prepared := f.PrepareOnly(t, child)
	var calls atomic.Int32
	var feedbackSeen, parentSeen atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/readyz" {
			io.WriteString(w, `{"ready":true}`)
			return
		}
		b, e := io.ReadAll(io.LimitReader(r.Body, 131073))
		if e != nil || len(b) > 131072 || !json.Valid(b) {
			w.WriteHeader(400)
			return
		}
		n := calls.Add(1)
		if strings.Contains(string(b), "Add a boundary regression test.") {
			feedbackSeen.Store(true)
		}
		if strings.Contains(string(b), "unvalidated") {
			parentSeen.Store(true)
		}
		message := map[string]any{"role": "assistant", "content": "Repair fixture complete."}
		finish := "stop"
		if n < 3 {
			name, args := "read_file", `{"path":"candidate-fixture.txt"}`
			if n == 2 {
				name, args = "write_file", `{"path":"repair-fixture.txt","content":"repaired\n"}`
			}
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"id": "repair-fixture", "type": "function", "function": map[string]string{"name": name, "arguments": args}}}
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"model": "codex-cli", "choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}}})
	}))
	defer server.Close()
	provider, e := modelbroker.NewProvider(modelbroker.ProviderConfig{Endpoint: server.URL + "/v1/chat/completions", BearerToken: strings.Repeat("x", 48), AllowLoopbackHTTP: true})
	if e != nil {
		t.Fatal(e)
	}
	client, e := podtransport.NewClient(in.Config)
	if e != nil {
		t.Fatal(e)
	}
	record, e := execution.RunController(ctx, f.Worker, f.LoadLifecycle, prepared, client, provider, func(context.Context) (execution.ModelPolicy, error) { return f.Runtime.Model, nil })
	if e != nil {
		t.Fatal("actual child controller", execution.DiagnosticStage(e), "calls", calls.Load())
	}
	if record.RequestID != child || record.Changes != 2 || record.ModelCalls != 3 || calls.Load() != 3 || !feedbackSeen.Load() || !parentSeen.Load() {
		t.Fatal("repair did not preserve parent and consume private feedback", record.Changes, record.ModelCalls, calls.Load(), feedbackSeen.Load(), parentSeen.Load())
	}
	var after, state, reason, cleanup string
	if e = f.Admin.Pool.QueryRow(`SELECT row_to_json(b)::text FROM model_budgets b`).Scan(&after); e != nil || after != original {
		t.Fatal("parent budget changed", e)
	}
	if e = f.Admin.Pool.QueryRow(`SELECT r.state,r.stop_reason,l.cleanup_state FROM execution_requests r JOIN execution_lifecycles l ON l.request_id=r.id WHERE r.id=$1`, child).Scan(&state, &reason, &cleanup); e != nil || state != "stopped" || reason != "candidate-captured" || cleanup != "complete" {
		t.Fatal("child not retired", state, reason, cleanup, e)
	}
	report, _ := json.Marshal(map[string]any{"scope": "isolated-connected-repair-controller", "syntheticParentHistory": true, "syntheticSource": true, "syntheticModel": true, "realPostgres": true, "realKubernetes": true, "realSCC": true, "signedRepairHandoff": true, "signedOrchestratorInbox": true, "signedChildInbox": true, "parentBudgetUnchanged": true, "privateFeedbackConsumed": true, "rejectedTreeImported": true, "cumulativeChanges": record.Changes, "childModelCalls": record.ModelCalls, "candidateDeltaSHA256": record.DeltaSHA256, "assignmentSHA256": authority.Digest(prepared.Assignment()), "childCleanupComplete": true, "autonomousE2E": false})
	if e = os.WriteFile(in.ReportFile, report, 0600); e != nil {
		t.Fatal("report")
	}
}
