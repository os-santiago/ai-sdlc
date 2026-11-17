package execution

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func capturedCleanupFixture(t *testing.T) (*lifecycleFixture, *Lifecycle, store.CandidateRecord, string, string) {
	t.Helper()
	f, l, _, raw := candidateFixture(t)
	record, err := l.CaptureCandidate(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Admin.Pool.Exec(`GRANT UPDATE(cleanup_state) ON execution_lifecycles TO aisdlc_worker`); err != nil {
		t.Fatal(err)
	}
	var config, pod string
	if err = f.Admin.Pool.QueryRow(`SELECT config_uid,pod_uid FROM execution_lifecycles WHERE request_id=$1`, record.RequestID).Scan(&config, &pod); err != nil {
		t.Fatal(err)
	}
	return f, l, record, config, pod
}

func cleanupState(t *testing.T, f *lifecycleFixture, id string) (string, string, string) {
	t.Helper()
	var state, reason, cleanup string
	if err := f.Admin.Pool.QueryRow(`SELECT r.state,r.stop_reason,l.cleanup_state FROM execution_requests r JOIN execution_lifecycles l ON l.request_id=r.id WHERE r.id=$1`, id).Scan(&state, &reason, &cleanup); err != nil {
		t.Fatal(err)
	}
	return state, reason, cleanup
}

func TestCandidateCleanupClosesExactlyOwnedAttempt(t *testing.T) {
	f, l, record, config, pod := capturedCleanupFixture(t)
	ctx := context.Background()
	child := "execution-" + authority.Digest([]byte(store.NewID()))
	insertChild := `INSERT INTO execution_requests(id,work_item_uuid,attempt,repository,authority_revision,payload_digest,payload,receipt,expires_at) SELECT $2,work_item_uuid,2,repository,authority_revision,payload_digest,payload,receipt,expires_at FROM execution_requests WHERE id=$1`
	_, blocked := f.Admin.Pool.Exec(insertChild, record.RequestID, child)
	var code interface{ SQLState() string }
	if !errors.As(blocked, &code) || code.SQLState() != "23505" {
		t.Fatal("live parent did not exclude another attempt", blocked)
	}
	var before string
	if err := f.Admin.Pool.QueryRow(`SELECT row_to_json(b)::text FROM model_budgets b WHERE work_item_uuid=$1`, f.command.WorkItemID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if f.db.ConfirmExecutionCleanup(ctx, l.durable, store.NewID(), pod) == nil {
		t.Fatal("foreign config acknowledged")
	}
	if state, _, cleanup := cleanupState(t, f, record.RequestID); state != "claimed" || cleanup != "pending" {
		t.Fatal("failed cleanup released attempt")
	}
	if err := f.db.ConfirmExecutionCleanup(ctx, l.durable, config, pod); err != nil {
		t.Fatal(err)
	}
	if state, reason, cleanup := cleanupState(t, f, record.RequestID); state != "stopped" || reason != "candidate-captured" || cleanup != "complete" {
		t.Fatal("candidate cleanup did not atomically close owner", state, reason, cleanup)
	}
	if _, err := f.db.ReadClaimedExecution(ctx, l.durable.Claim); err == nil {
		t.Fatal("closed owner retained authority")
	}
	var after string
	if err := f.Admin.Pool.QueryRow(`SELECT row_to_json(b)::text FROM model_budgets b WHERE work_item_uuid=$1`, f.command.WorkItemID).Scan(&after); err != nil || before != after {
		t.Fatal("cleanup changed original budget", err)
	}
	// Exercise only the database uniqueness boundary with explicitly synthetic
	// bytes; this is not an authenticated repair command or a workflow run.
	if _, err := f.Admin.Pool.Exec(insertChild, record.RequestID, child); err != nil {
		t.Fatal("closed candidate still occupies active attempt slot", err)
	}
	if f.db.StopExecutionClaim(ctx, l.durable.Claim, "candidate-captured") == nil {
		t.Fatal("generic stop acquired successful closure")
	}
}

func TestCandidateCleanupFailureAndLostCommit(t *testing.T) {
	for _, mode := range []string{"closure-failure", "lost-ack"} {
		t.Run(mode, func(t *testing.T) {
			f, l, record, config, pod := capturedCleanupFixture(t)
			ctx := context.Background()
			if mode == "closure-failure" {
				if _, err := f.Admin.Pool.Exec(`CREATE FUNCTION fail_candidate_close() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.stop_reason='candidate-captured' THEN RAISE EXCEPTION 'fixture closure denied'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_candidate_close BEFORE UPDATE ON execution_requests FOR EACH ROW EXECUTE FUNCTION fail_candidate_close()`); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_, _ = f.Admin.Pool.Exec(`DROP TRIGGER IF EXISTS fail_candidate_close ON execution_requests; DROP FUNCTION IF EXISTS fail_candidate_close()`)
				})
				if f.db.ConfirmExecutionCleanup(ctx, l.durable, config, pod) == nil {
					t.Fatal("closure failure ignored")
				}
				if state, _, cleanup := cleanupState(t, f, record.RequestID); state != "claimed" || cleanup != "pending" {
					t.Fatal("closure failure committed partial cleanup")
				}
				return
			}
			fault, dropped := executionCommitFault(t, 1)
			if fault.ConfirmExecutionCleanup(ctx, l.durable, config, pod) == nil {
				t.Fatal("lost ACK reported confirmed success")
			}
			select {
			case <-dropped:
			default:
				t.Fatal("real server COMMIT acknowledgement was not dropped")
			}
			if state, reason, cleanup := cleanupState(t, f, record.RequestID); state != "stopped" || reason != "candidate-captured" || cleanup != "complete" {
				t.Fatal("lost ACK lost committed fact")
			}
			var before, after string
			if err := f.Admin.Pool.QueryRow(`SELECT row_to_json(r)::text FROM execution_requests r WHERE id=$1`, record.RequestID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err := f.db.ConfirmExecutionCleanup(ctx, l.durable, config, pod); err != nil {
				t.Fatal("exact closure reobservation failed", err)
			}
			if err := f.Admin.Pool.QueryRow(`SELECT row_to_json(r)::text FROM execution_requests r WHERE id=$1`, record.RequestID).Scan(&after); err != nil || after != before {
				t.Fatal("closure replay changed original request", err)
			}
		})
	}
}

func TestCandidateCleanupExpiryAfterUpdatePreservesOnlyContainment(t *testing.T) {
	f, l, record, config, pod := capturedCleanupFixture(t)
	// Sequence advancement survives the savepoint rollback and proves that the
	// AFTER UPDATE path actually executed, rather than an already-expired WHERE.
	if _, err := f.Admin.Pool.Exec(`CREATE SEQUENCE candidate_close_trigger_calls;
 GRANT USAGE ON SEQUENCE candidate_close_trigger_calls TO aisdlc_worker;
 CREATE FUNCTION delay_candidate_close() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.stop_reason='candidate-captured' THEN PERFORM nextval('candidate_close_trigger_calls'); PERFORM pg_sleep(1.1); END IF; RETURN NEW; END $$;
 CREATE TRIGGER delay_candidate_close AFTER UPDATE ON execution_requests FOR EACH ROW EXECUTE FUNCTION delay_candidate_close()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.Admin.Pool.Exec(`DROP TRIGGER IF EXISTS delay_candidate_close ON execution_requests; DROP FUNCTION IF EXISTS delay_candidate_close(); DROP SEQUENCE IF EXISTS candidate_close_trigger_calls`)
	})
	l.durable.Claim.Until = time.Now().Add(time.Second).Truncate(time.Microsecond)
	if _, err := f.Admin.Pool.Exec(`UPDATE execution_requests SET claim_until=$2 WHERE id=$1`, record.RequestID, l.durable.Claim.Until); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Admin.Pool.Exec(`UPDATE execution_lifecycles SET claim_until=$2 WHERE request_id=$1`, record.RequestID, l.durable.Claim.Until); err != nil {
		t.Fatal(err)
	}
	if err := f.db.ConfirmExecutionCleanup(context.Background(), l.durable, config, pod); err != nil {
		t.Fatal(err)
	}
	var called bool
	var calls int
	if err := f.Admin.Pool.QueryRow(`SELECT last_value,is_called FROM candidate_close_trigger_calls`).Scan(&calls, &called); err != nil || !called || calls != 1 {
		t.Fatal("AFTER UPDATE did not execute exactly once", err)
	}
	if state, reason, cleanup := cleanupState(t, f, record.RequestID); state != "claimed" || reason != "" || cleanup != "complete" {
		t.Fatal("late closure was mistaken for success or cleanup was discarded", state, reason, cleanup)
	}
}

func TestCandidateCleanupAndExpiryRecoveryPreserveTerminalReason(t *testing.T) {
	f, l, record, config, pod := capturedCleanupFixture(t)
	passwords := map[string]string{}
	for role := range f.Material.Identities {
		passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
	}
	if err := f.Admin.ProvisionRolesForOptions(context.Background(), passwords, "verification", true); err != nil {
		t.Fatal(err)
	}
	l.durable.Claim.Until = time.Now().Add(-time.Second).Truncate(time.Microsecond)
	if _, err := f.Admin.Pool.Exec(`UPDATE execution_requests SET claim_until=$2 WHERE id=$1`, record.RequestID, l.durable.Claim.Until); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Admin.Pool.Exec(`UPDATE execution_lifecycles SET claim_until=$2 WHERE request_id=$1`, record.RequestID, l.durable.Claim.Until); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { done <- f.db.ConfirmExecutionCleanup(context.Background(), l.durable, config, pod) }()
	go func() { done <- f.db.ExpireExecutionClaims(context.Background()) }()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	// The expiry selector deliberately uses SKIP LOCKED. If containment held
	// the request, the next ordinary sweep must finish terminal reconciliation.
	if err := f.db.ExpireExecutionClaims(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state, reason, cleanup := cleanupState(t, f, record.RequestID); state != "stopped" || reason != "expired" || cleanup != "complete" {
		t.Fatal("concurrent containment rewrote terminal expiry", state, reason, cleanup)
	}
}
