package execution

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"strings"
	"testing"
)

func candidatePrivileges(t *testing.T, f *fixture) {
	t.Helper()
	passwords := map[string]string{}
	for role := range f.Material.Identities {
		passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
	}
	if err := f.Admin.ProvisionRolesForMode(context.Background(), passwords, "candidate"); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredOwnerCannotResumeAndCleanupHasOnlyAcknowledgedUIDs(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "acknowledged", true: "lost-create-ack"}[lost], func(t *testing.T) {
			f := newLifecycleFixture(t)
			candidatePrivileges(t, f.preparationFixture.fixture)
			f.placement.Namespace = "ai-sdlc-executors"
			l := f.open(t)
			ctx := context.Background()
			uid := store.NewID()
			if err := l.RunEffect(ctx, "config", func(context.Context, EffectInput) (store.LifecycleResult, error) {
				return store.LifecycleResult{UID: uid}, nil
			}); err != nil {
				t.Fatal(err)
			}
			if lost {
				if _, err := f.db.BeginLifecycleEffect(ctx, l.durable, "pod"); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.db.ExpireExecutionClaims(ctx); err != nil {
				t.Fatal(err)
			}
			if r, err := f.db.NextExecutionCleanup(ctx); err != nil || r.RequestID != "" {
				t.Fatal("active owner cleanup", err)
			}
			// Test-only clock control in a disposable DB; production expiry is immutable.
			if _, err := f.Admin.Pool.Exec(`UPDATE execution_requests SET claim_until=clock_timestamp()-interval '1 second'`); err != nil {
				t.Fatal(err)
			}
			if err := f.db.ExpireExecutionClaims(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.ReadClaimedExecution(ctx, l.durable.Claim); err == nil {
				t.Fatal("expired owner adopted")
			}
			r, err := f.db.NextExecutionCleanup(ctx)
			if err != nil || r.ConfigUID != uid || r.PodUID != "" || r.Uncertain != lost {
				t.Fatal("cleanup identity or ambiguity lost", err)
			}
			if err = f.db.FinishExecutionCleanup(ctx, r, true); err != nil {
				t.Fatal(err)
			}
			blocked, err := f.db.ExecutionCleanupBlocked(ctx)
			if err != nil || blocked != lost {
				t.Fatal("ambiguous create must block further execution")
			}
			if again, err := f.db.NextExecutionCleanup(ctx); err != nil || again.RequestID != "" {
				t.Fatal("terminal cleanup replayed")
			}
		})
	}
}

func TestExpiredModelOwnerStopsBudgetWithoutRefund(t *testing.T) {
	f, l, load, pod := modelLifecycleFixture(t)
	candidatePrivileges(t, f.preparationFixture.fixture)
	ctx := context.Background()
	scope, err := l.ActivateModel(ctx, load)
	if err != nil {
		t.Fatal(err)
	}
	startModelLifecycle(t, l, pod)
	// Represent a provider call in flight at process loss, without invoking a model.
	if _, err = f.Admin.Pool.Exec(`INSERT INTO model_calls(allocation_id,sequence,request_sha256,input_bytes,response_limit,expires_at,state) VALUES($1,1,$2,10,100,clock_timestamp()+interval '1 hour','started');`, scope.AllocationID, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Admin.Pool.Exec(`UPDATE model_budgets SET reserved_calls=1,reserved_input_bytes=10,reserved_output_bytes=100; UPDATE execution_requests SET claim_until=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if err = f.db.ExpireExecutionClaims(ctx); err != nil {
		t.Fatal(err)
	}
	if f.Count(t, `SELECT count(*) FROM model_budgets WHERE state='stopped' AND reserved_calls=1 AND reserved_output_bytes=100`) != 1 || f.Count(t, `SELECT count(*) FROM model_calls WHERE state='ambiguous'`) != 1 {
		t.Fatal("recovery reopened or refunded provider call")
	}
	if _, err = l.Authorize(ctx, scope); err == nil {
		t.Fatal("late provider remains authorized")
	}
}

func TestStoppedControllerIsTerminalizedAndCleanupCooldownBlocks(t *testing.T) {
	for _, handed := range []bool{false, true} {
		t.Run(map[bool]string{false: "activated", true: "handed-off"}[handed], func(t *testing.T) {
			f, l, load, pod := modelLifecycleFixture(t)
			candidatePrivileges(t, f.preparationFixture.fixture)
			ctx := context.Background()
			scope, err := l.ActivateModel(ctx, load)
			if err != nil {
				t.Fatal(err)
			}
			startModelLifecycle(t, l, pod)
			if handed {
				if _, err = f.Admin.Pool.Exec(`INSERT INTO model_calls(allocation_id,sequence,request_sha256,input_bytes,response_limit,expires_at,state,output_bytes,response_sha256,response_status,response_handed_off_at) VALUES($1,1,$2,10,100,clock_timestamp()+interval '1 hour','completed',10,$2,200,clock_timestamp())`, scope.AllocationID, strings.Repeat("a", 64)); err != nil {
					t.Fatal(err)
				}
			}
			if err = f.db.StopExecutionClaim(ctx, l.durable.Claim, "uncertain"); err != nil {
				t.Fatal(err)
			}
			if err = f.db.ExpireExecutionClaims(ctx); err != nil {
				t.Fatal(err)
			}
			if f.Count(t, `SELECT count(*) FROM work_items WHERE status='failed'`) != 1 || f.Count(t, `SELECT count(*) FROM model_budgets WHERE state='stopped'`) != 1 {
				t.Fatal("stopped controller remained active")
			}
			if handed && f.Count(t, `SELECT count(*) FROM model_calls WHERE state='completed' AND response_handed_off_at IS NOT NULL`) != 1 {
				t.Fatal("confirmed handoff evidence changed")
			}
			r, err := f.db.NextExecutionCleanup(ctx)
			if err != nil || r.RequestID == "" {
				t.Fatal("no cleanup")
			}
			if err = f.db.FinishExecutionCleanup(ctx, r, false); err != nil {
				t.Fatal(err)
			}
			if pending, err := f.db.NextExecutionCleanup(ctx); err != nil || pending.RequestID != "" {
				t.Fatal("cleanup ignores cooldown")
			}
			if blocked, err := f.db.ExecutionCleanupBlocked(ctx); err != nil || !blocked {
				t.Fatal("cooldown admitted new work")
			}
			if err = f.db.ExpireExecutionClaims(ctx); err != nil {
				t.Fatal("terminalization is not idempotent", err)
			}
		})
	}
}
