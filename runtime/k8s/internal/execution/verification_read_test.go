package execution

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestIndependentRoleReadsOnlyCleanedCandidateLedger(t *testing.T) {
	f, l, _, raw := candidateFixture(t)
	record, err := l.CaptureCandidate(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("fixture URL")
	}
	u.User = url.UserPassword("aisdlc_verifier", "verifier"+strings.Repeat("q", 40))
	verifier, err := store.Open(context.Background(), u.String())
	if err != nil {
		t.Fatal("verifier fixture connection")
	}
	t.Cleanup(func() { verifier.Close() })
	ctx := context.Background()
	if _, err = verifier.ReadVerificationCandidate(ctx, record.RequestID); err == nil {
		t.Fatal("inactive verifier privileges enabled")
	}
	// Explicit fixture-only activation. The normal provisioner still grants none.
	if _, err = f.Admin.Pool.Exec(`GRANT SELECT ON execution_candidates,execution_requests,execution_lifecycles,execution_models,model_allocations,model_budgets,execution_repairs,repair_model_budgets TO aisdlc_verifier; GRANT UPDATE(cleanup_state) ON execution_lifecycles TO aisdlc_worker`); err != nil {
		t.Fatal(err)
	}
	if _, err = verifier.ReadVerificationCandidate(ctx, record.RequestID); err == nil {
		t.Fatal("uncleaned candidate accepted")
	}
	var config, pod string
	if err = f.Admin.Pool.QueryRow(`SELECT config_uid,pod_uid FROM execution_lifecycles WHERE request_id=$1`, record.RequestID).Scan(&config, &pod); err != nil {
		t.Fatal(err)
	}
	if err = f.db.ConfirmExecutionCleanup(ctx, l.durable, config, pod); err != nil {
		t.Fatal(err)
	}
	row, err := verifier.ReadVerificationCandidate(ctx, record.RequestID)
	if err != nil || row.Record != record || row.WorkItem == "" || row.Repository != f.inputs.Policy.Repository {
		t.Fatal("independent reader", err)
	}
	if len(row.Payload) == 0 || len(row.Receipt) == 0 || len(row.Delta) == 0 {
		t.Fatal("immutable evidence missing")
	}
	for _, db := range []*store.DB{f.db, f.Admin, f.Components["orchestrator"].DB} {
		if _, err = db.ReadVerificationCandidate(ctx, record.RequestID); err == nil {
			t.Fatal("wrong principal read")
		}
	}
	if _, err = verifier.Pool.Exec(`UPDATE execution_candidates SET changes=changes+1`); err == nil {
		t.Fatal("reader mutated candidate")
	}
	if _, err = f.Admin.Pool.Exec(`UPDATE model_budgets SET stop_reason='uncertain'`); err != nil {
		t.Fatal(err)
	}
	if _, err = verifier.ReadVerificationCandidate(ctx, record.RequestID); err == nil {
		t.Fatal("uncertain model accepted")
	}
}
