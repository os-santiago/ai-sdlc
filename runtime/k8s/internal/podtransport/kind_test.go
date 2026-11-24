package podtransport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// This test is driven only by tools/verify_pod_transport.py against disposable
// fixture workloads, a real PostgreSQL fixture and synthetic model responses.
// Its private input file contains fixture control-plane credentials; errors and
// test output deliberately never include that file, API errors or payloads.
func TestRealKubernetesMailboxFixture(t *testing.T) {
	path := os.Getenv("AI_SDLC_POD_TRANSPORT_FIXTURE")
	if path == "" {
		t.Skip("isolated kind fixture required")
	}
	raw, err := os.ReadFile(path)
	var input struct {
		Config                                Config
		Binding                               Binding
		Scenario, DatabaseURL, ReplacementUID string
	}
	if err != nil || len(raw) > 256*1024 || json.Unmarshal(raw, &input) != nil {
		t.Fatal("invalid private fixture input")
	}
	// The local kind fixtures use the installer's default sandbox class.
	if input.Binding.RuntimeClass == "" {
		input.Binding.RuntimeClass = executor.DefaultRuntimeClass
	}
	client, err := NewClient(input.Config)
	if err != nil {
		t.Fatal("fixture client rejected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	session, err := Bind(ctx, client, input.Binding)
	if input.Scenario == "replaced" {
		if err == nil {
			t.Fatal("replaced real Pod accepted")
		}
		if input.ReplacementUID == "" || input.ReplacementUID == input.Binding.PodUID {
			t.Fatal("replacement identity not independently observed")
		}
		input.Binding.PodUID = input.ReplacementUID
		if _, err := Bind(ctx, client, input.Binding); err != nil {
			t.Fatal("replacement fixture is not otherwise a valid running Pod")
		}
		t.Log("actual replacement UID rejected; no helper invoked")
		return
	}
	if err != nil {
		t.Fatal("actual Pod binding rejected")
	}
	if input.Scenario == "permission-denied" {
		if _, err := session.ReadRequest(ctx); !errors.Is(err, ErrPermission) {
			t.Fatal("RBAC-denied helper accepted")
		}
		t.Log("actual read-only principal could observe Pod but helper exec was denied")
		return
	}
	if input.Scenario != "normal" {
		t.Fatal("unsupported fixture scenario")
	}
	a, err := executor.ParseAssignment(input.Binding.Assignment, modelmailbox.Digest(input.Binding.Assignment), time.Now())
	if err != nil {
		t.Fatal("fixture assignment")
	}
	db, err := store.Open(ctx, input.DatabaseURL)
	if err != nil {
		t.Fatal("fixture PostgreSQL unavailable")
	}
	defer db.Close()
	if db.Migrate(ctx) != nil {
		t.Fatal("fixture migration failed")
	}
	plan := store.NewID()
	if store.UpsertPlan(ctx, db.Pool, store.PlanRecord{ID: plan, Repository: a.Repository, CorrelationID: store.NewID()}) != nil {
		t.Fatal("fixture plan")
	}
	if store.UpsertWorkItem(ctx, db.Pool, store.WorkItemRecord{PlanID: plan, ID: "fixture", UUID: a.WorkItemID, Status: "running", IssueNumbers: []int{1}, AcceptanceCriteria: []string{"isolated fixture"}}) != nil {
		t.Fatal("fixture work item")
	}
	budget := store.ModelBudget{WorkItem: a.WorkItemID, Repository: a.Repository, SourceSHA: a.SourceSHA, PolicyDigest: a.ProfileSHA256, Model: "fixture-model", Limits: store.ModelLimits{Calls: 4, InputBytes: 512 * 1024, OutputBytes: 64 * 1024}, ExpiresAt: time.Now().Add(30 * time.Minute)}
	if db.CreateModelBudget(ctx, budget) != nil {
		t.Fatal("fixture budget")
	}
	allocation := store.ModelAllocation{ID: store.NewID(), WorkItem: a.WorkItemID, PodUID: input.Binding.PodUID, Attempt: a.Attempt, Limits: budget.Limits, ResponseLimit: 16 * 1024, ExpiresAt: time.Now().Add(20 * time.Minute)}
	if db.CreateModelAllocation(ctx, allocation) != nil {
		t.Fatal("fixture allocation")
	}
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, io.LimitReader(r.Body, modelmailbox.MaxRequestBytes+1)); err != nil {
			w.WriteHeader(400)
			return
		}
		n := calls.Add(1)
		message := map[string]any{"role": "assistant", "content": "Created the synthetic fixture file."}
		reason := "stop"
		if n == 1 {
			arguments, _ := json.Marshal(map[string]string{"path": "AI_SDLC_EXECUTOR_FIXTURE.txt", "content": "Created by the isolated SCC executor conformance fixture.\n"})
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "fixture-write", "type": "function", "function": map[string]any{"name": "write_file", "arguments": string(arguments)}}}}
			reason = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "object": "chat.completion", "model": "fixture-model", "choices": []any{map[string]any{"index": 0, "finish_reason": reason, "message": message}}})
	}))
	defer provider.Close()
	p, err := modelbroker.NewProvider(modelbroker.ProviderConfig{Endpoint: provider.URL + "/v1/chat/completions", BearerToken: strings.Repeat("x", 32), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal("fixture provider")
	}
	dispatcher := modelbroker.Dispatcher{DB: db, Provider: p, Mailbox: session, Authority: syntheticFixtureAuthority{}}
	seen := map[uint64]bool{}
	for len(seen) < 2 {
		request, err := session.ReadRequest(ctx)
		if errors.Is(err, modelmailbox.ErrPending) {
			select {
			case <-time.After(50 * time.Millisecond):
				continue
			case <-ctx.Done():
				t.Fatal("fixture request timeout")
			}
		}
		if err != nil {
			t.Fatal("actual mailbox read failed")
		}
		if seen[request.Sequence] {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		result, err := dispatcher.Deliver(ctx, allocation.ID, input.Binding.Profile, request)
		if err != nil || !result.DispatchAttempted || !result.HandoffConfirmed {
			t.Fatalf("actual broker/publication handoff failed: sequence=%d attempted=%t confirmed=%t providerCalls=%d outcome=%s", request.Sequence, result.DispatchAttempted, result.HandoffConfirmed, calls.Load(), result.Outcome)
		}
		seen[request.Sequence] = true
		duplicate, err := dispatcher.Deliver(ctx, allocation.ID, input.Binding.Profile, request)
		if err != nil || duplicate.DispatchAttempted || !duplicate.HandoffConfirmed {
			t.Fatal("confirmed dispatch metadata replay failed")
		}
	}
	var completed, handed int
	if db.Pool.QueryRowContext(ctx, `SELECT count(*),count(response_handed_off_at) FROM model_calls WHERE allocation_id=$1 AND state='completed'`, allocation.ID).Scan(&completed, &handed) != nil || completed != 2 || handed != 2 || calls.Load() != 2 {
		t.Fatal("real ledger/provider counts do not match handoffs")
	}
	t.Log("two actual SCC mailbox requests: two synthetic HTTP provider calls, two durable completed handoffs, duplicate delivery metadata only")
}

// Only isolated fixtures use this synthetic authority; it is not evidence of
// signed live workflow permission. Production controllers need current proof.
type syntheticFixtureAuthority struct{}

func (syntheticFixtureAuthority) Authorize(ctx context.Context, scope store.ModelScope) (time.Time, error) {
	return scope.ExpiresAt, ctx.Err()
}
