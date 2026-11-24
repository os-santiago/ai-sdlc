package execution_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repairflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func repairLoad(f execution.RepairFixtureForTest, role string) repairflow.Loader {
	return func(context.Context) (repairflow.Inputs, error) {
		return repairflow.Inputs{Policy: f.Policy, Identity: f.Identities[role], Intake: f.Config,
			Runtime: f.Runtime, Profile: f.Profile, ReviewProfile: f.Review.ProfileDigest, ReviewConfiguration: f.Review.ConfigurationDigest}, nil
	}
}

// Each envelope is relayed from the actual committed outbox receipt. No test
// calls the orchestrator effect with an invented receipt or bypasses its inbox.
func relayRepair(t *testing.T, f execution.RepairFixtureForTest, operation string, load repairflow.Loader) (bool, error) {
	t.Helper()
	var payload, raw []byte
	if e := f.Admin.Pool.QueryRow(`SELECT payload,receipt FROM authorized_outbox WHERE operation=$1 ORDER BY created_at DESC LIMIT 1`, operation).Scan(&payload, &raw); e != nil {
		t.Fatal(e)
	}
	var receipt authority.Receipt
	if json.Unmarshal(raw, &receipt) != nil {
		t.Fatal("receipt")
	}
	path := "/internal/" + operation
	transport, e := f.Identities["relay"].Carry(f.Policy, receipt, http.MethodPost, path, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	envelope := delivery.Envelope{Receipt: receipt, Transport: transport, Payload: payload}
	target, db, effect := "orchestrator", f.Orchestrator, repairflow.Effect(load)
	if operation == "execute-work" {
		target, db, effect = "worker", f.Worker, execution.Effect(func() (authority.Policy, error) { return f.Policy, nil })
	}
	if operation == "verify-artifact" {
		target, db, effect = "verifier", f.Verifier, verificationflow.Effect(func() (authority.Policy, error) { return f.Policy, nil })
	}
	if operation == "stop-rejected-work" {
		effect = repairflow.StopEffect(func() (authority.Policy, error) { return f.Policy, nil })
	}
	_, duplicate, e := (delivery.Inbox{DB: db, Target: target}).Accept(context.Background(), f.Policy, envelope, operation, http.MethodPost, path, time.Now(), effect)
	return duplicate, e
}

func TestConnectedRepairDerivesOneChildAndPreparesRejectedTree(t *testing.T) {
	t.Run("changed", func(t *testing.T) { connectedRepairCapture(t, false) })
	t.Run("unchanged-rejected", func(t *testing.T) { connectedRepairCapture(t, true) })
}
func connectedRepairCapture(t *testing.T, unchanged bool) {
	f := execution.NewRepairFixtureForTest(t, true)
	ctx := context.Background()
	var original string
	if e := f.Admin.Pool.QueryRow(`SELECT row_to_json(b)::text FROM model_budgets b WHERE work_item_uuid=$1`, f.Command.WorkItemID).Scan(&original); e != nil {
		t.Fatal(e)
	}
	if sent, e := repairflow.Handoff(ctx, f.Verifier, repairLoad(f, "verifier")); e != nil || !sent {
		t.Fatal("handoff", sent, e)
	}
	if duplicate, e := relayRepair(t, f, "repair-candidate", repairLoad(f, "orchestrator")); e != nil || duplicate {
		t.Fatal("derive", duplicate, e)
	}
	if duplicate, e := relayRepair(t, f, "repair-candidate", repairLoad(f, "orchestrator")); e != nil || !duplicate {
		t.Fatal("duplicate derive", duplicate, e)
	}
	if sent, e := repairflow.Handoff(ctx, f.Verifier, repairLoad(f, "verifier")); e != nil || sent {
		t.Fatal("repeated handoff", sent, e)
	}
	var child string
	var calls int
	var input, output int64
	var expires, parentExpiry time.Time
	if e := f.Admin.Pool.QueryRow(`SELECT r.child_request_id,b.max_calls,b.max_input_bytes,b.max_output_bytes,r.expires_at,p.expires_at FROM execution_repairs r JOIN repair_model_budgets b USING(work_item_uuid) JOIN model_budgets p USING(work_item_uuid)`).Scan(&child, &calls, &input, &output, &expires, &parentExpiry); e != nil {
		t.Fatal(e)
	}
	if calls != 3 || input != f.Runtime.Model.Limits.InputBytes-2 || output != f.Runtime.Model.Limits.OutputBytes-32 || expires.After(parentExpiry) {
		t.Fatal("remaining allowance changed", calls, input, output, expires, parentExpiry)
	}
	if _, e := relayRepair(t, f, "execute-work", nil); e != nil {
		t.Fatal("child inbox", e)
	}
	p, l := f.Prepare(t, child)
	a, e := executor.ParseAssignment(p.Assignment(), authority.Digest(p.Assignment()), time.Now())
	if e != nil || a.Repair == nil || a.Repair.RemainingCalls != 3 || a.Repair.Reference.ParentCandidateID != f.Command.ID() {
		t.Fatal("repair assignment", e)
	}
	bundle, e := workspacebundle.Decode(p.Source())
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, file := range bundle.Files {
		if file.Path == "candidate-fixture.txt" && string(file.Data) == "unvalidated\n" {
			found = true
		}
	}
	if !found {
		t.Fatal("rejected candidate was not imported")
	}
	pod := store.NewID()
	for _, kind := range []string{"config", "pod", "source"} {
		if e := l.RunEffect(ctx, kind, func(_ context.Context, in execution.EffectInput) (store.LifecycleResult, error) {
			r := store.LifecycleResult{UID: pod}
			if kind == "config" {
				r.UID = store.NewID()
			}
			if kind == "source" {
				r.Digest = authority.Digest(in.Source)
				r.Bytes = len(in.Source)
			}
			return r, nil
		}); e != nil {
			t.Fatal(kind, e)
		}
	}
	scope, e := l.ActivateModel(ctx, func(context.Context) (execution.ModelPolicy, error) { return f.Runtime.Model, nil })
	if e != nil || scope.Attempt != 2 || scope.ExpiresAt.After(expires) {
		t.Fatal("derived model activation", e)
	}
	if e = l.RunEffect(ctx, "start", func(context.Context, execution.EffectInput) (store.LifecycleResult, error) {
		return store.LifecycleResult{UID: pod}, nil
	}); e != nil {
		t.Fatal(e)
	}
	request := []byte(`{"model":"fixture","messages":[{"role":"user","content":"repair fixture"}]}`)
	reserved, e := f.Worker.ReserveModelCall(ctx, scope.AllocationID, pod, 1, request)
	if e != nil || reserved.Reason != "reserved" {
		t.Fatal("child reservation", e)
	}
	ref := reserved.Call.ModelCallRef
	if decision, e := f.Worker.ClaimModelCall(ctx, ref); e != nil || !decision.Invoke {
		t.Fatal("child dispatch", e)
	}
	response := []byte(`{"choices":[{"message":{"role":"assistant","content":"fixture repaired"}}]}`)
	if _, e = f.Worker.FinishModelCall(ctx, ref, 200, response); e != nil {
		t.Fatal(e)
	}
	if _, e = f.Worker.AcknowledgeModelResponse(ctx, ref, authority.Digest(response), 200, len(response)); e != nil {
		t.Fatal(e)
	}
	if !unchanged {
		bundle.Files = append(bundle.Files, workspacebundle.File{Path: "repair-fixture.txt", Mode: 0644, Data: []byte("repaired\n")})
	}
	exported, e := workspacebundle.Encode(bundle)
	if e != nil {
		t.Fatal(e)
	}
	zero := int32(0)
	if e = l.RunEffect(ctx, "export", func(context.Context, execution.EffectInput) (store.LifecycleResult, error) {
		return store.LifecycleResult{UID: pod, Digest: authority.Digest(exported), Bytes: len(exported), ExitCode: &zero}, nil
	}); e != nil {
		t.Fatal(e)
	}
	captured, e := l.CaptureCandidate(ctx, exported)
	if unchanged {
		if e == nil {
			t.Fatal("unchanged candidate entered another independent review")
		}
		var n int
		if e := f.Admin.Pool.QueryRow(`SELECT count(*) FROM execution_candidates`).Scan(&n); e != nil || n != 1 {
			t.Fatal("unchanged repair persisted", n, e)
		}
		return
	}

	if e != nil || captured.Changes != 2 || captured.ModelCalls != 1 || captured.SourceTreeSHA256 != a.Repair.Reference.OriginalTreeSHA256 {
		t.Fatal("cumulative capture", captured, e)
	}
	var deltaRaw []byte
	if e = f.Admin.Pool.QueryRow(`SELECT delta FROM execution_candidates WHERE request_id=$1`, child).Scan(&deltaRaw); e != nil {
		t.Fatal(e)
	}
	delta, e := candidate.Decode(deltaRaw)
	if e != nil {
		t.Fatal(e)
	}
	base, e := workspacebundle.Decode(f.OriginalSource)
	if e != nil {
		t.Fatal(e)
	}
	rebuilt, e := candidate.Apply(base, delta)
	if e != nil {
		t.Fatal(e)
	}
	rebuiltTree, _ := rebuilt.TreeDigest()
	expectedTree, _ := bundle.TreeDigest()
	if rebuiltTree != expectedTree {
		t.Fatal("repair delta lost the original candidate changes")
	}
	var after string
	if e = f.Admin.Pool.QueryRow(`SELECT row_to_json(b)::text FROM model_budgets b WHERE work_item_uuid=$1`, f.Command.WorkItemID).Scan(&after); e != nil || original != after {
		t.Fatal("parent budget changed", e)
	}

	execution.CleanupRepairForTest(t, f, l, child)
	sent, e := execution.HandoffCandidate(ctx, f.Worker, func() (authority.Policy, authority.Identity, intake.Config, string, error) {
		return f.Policy, f.Identities["worker"], f.Config, authority.Digest(f.Profile), nil
	})
	if e != nil || !sent {
		t.Fatal("child verification handoff", sent, e)
	}
	if _, e = relayRepair(t, f, "verify-artifact", nil); e != nil {
		t.Fatal("verification inbox", e)
	}
	var verificationID string
	if e = f.Admin.Pool.QueryRow(`SELECT id FROM verification_requests WHERE candidate_id=$1`, child).Scan(&verificationID); e != nil {
		t.Fatal(e)
	}
	review := execution.RejectRepairReviewForTest(t, f, verificationID)
	if sent, e = repairflow.Handoff(ctx, f.Verifier, repairLoad(f, "verifier")); e != nil || sent {
		t.Fatal("second rejection handoff", sent, e)
	}
	var outcome string
	if e = f.Admin.Pool.QueryRow(`SELECT outcome FROM review_repair_handoffs WHERE review_id=$1`, review.ID).Scan(&outcome); e != nil || outcome != "terminal-rejection" {
		t.Fatal("second rejection not terminal", outcome, e)
	}
	if _, e = relayRepair(t, f, "stop-rejected-work", nil); e != nil {
		t.Fatal("second rejection closure", e)
	}
	var work string
	var children int
	if e = f.Admin.Pool.QueryRow(`SELECT status,(SELECT count(*) FROM execution_repairs) FROM work_items WHERE uuid=$1`, f.Command.WorkItemID).Scan(&work, &children); e != nil || work != "failed" || children != 1 {
		t.Fatal("second rejection retried", work, children, e)
	}
}

func TestConnectedRepairObservationFailureDoesNotConsumeHandoff(t *testing.T) {
	f := execution.NewRepairFixtureForTest(t, true)
	ctx := context.Background()
	if _, e := f.Admin.Pool.Exec(`REVOKE SELECT ON model_calls FROM aisdlc_verifier`); e != nil {
		t.Fatal(e)
	}
	if sent, e := repairflow.Handoff(ctx, f.Verifier, repairLoad(f, "verifier")); sent || !errors.Is(e, store.ErrObservationUnavailable) {
		t.Fatal("observation failure became terminal", sent, e)
	}
	var count int
	if e := f.Admin.Pool.QueryRow(`SELECT count(*) FROM review_repair_handoffs`).Scan(&count); e != nil || count != 0 {
		t.Fatal("failed read consumed handoff", count, e)
	}
	if _, e := f.Admin.Pool.Exec(`GRANT SELECT ON model_calls TO aisdlc_verifier`); e != nil {
		t.Fatal(e)
	}
	// The final transactional proof must work with exactly one connection.
	f.Verifier.Pool.SetMaxOpenConns(1)
	if sent, e := repairflow.Handoff(ctx, f.Verifier, repairLoad(f, "verifier")); e != nil || !sent {
		t.Fatal("recovered single-connection handoff", sent, e)
	}
}

func TestConnectedRepairDerivationLostCommitAcknowledgement(t *testing.T) {
	f := execution.NewRepairFixtureForTest(t, true)
	if sent, e := repairflow.Handoff(context.Background(), f.Verifier, repairLoad(f, "verifier")); e != nil || !sent {
		t.Fatal(sent, e)
	}
	fault, dropped := execution.RepairCommitFaultForTest(t, "orchestrator")
	faultFixture := f
	faultFixture.Orchestrator = fault
	if _, e := relayRepair(t, faultFixture, "repair-candidate", repairLoad(f, "orchestrator")); e == nil {
		t.Fatal("lost COMMIT reply reported success")
	}
	select {
	case <-dropped:
	default:
		t.Fatal("server COMMIT acknowledgement not dropped")
	}
	if duplicate, e := relayRepair(t, f, "repair-candidate", repairLoad(f, "orchestrator")); e != nil || !duplicate {
		t.Fatal("durable child could not be observed by replay", duplicate, e)
	}
	var children, allowances, commands int
	if e := f.Admin.Pool.QueryRow(`SELECT (SELECT count(*) FROM execution_repairs),(SELECT count(*) FROM repair_model_budgets),(SELECT count(*) FROM authorized_outbox WHERE operation='execute-work')`).Scan(&children, &allowances, &commands); e != nil || children != 1 || allowances != 1 || commands != 1 {
		t.Fatal("duplicate child after lost acknowledgement", children, allowances, commands, e)
	}
}

func TestConnectedRepairWaitPastDeadlineRollsBackChildAndOutbox(t *testing.T) {
	f := execution.NewRepairFixtureForTest(t, true)
	// The sequence survives transaction rollback, proving the INSERT trigger
	// actually ran after the prechecks and before the final deadline fence.
	_, e := f.Admin.Pool.Exec(`CREATE SEQUENCE repair_wait_seen; GRANT USAGE ON SEQUENCE repair_wait_seen TO aisdlc_orchestrator;
 CREATE FUNCTION wait_repair_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM nextval('repair_wait_seen');PERFORM pg_sleep(1.5);RETURN NEW;END $$;
 CREATE TRIGGER wait_repair_insert AFTER INSERT ON execution_repairs FOR EACH ROW EXECUTE FUNCTION wait_repair_insert()`)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_, _ = f.Admin.Pool.Exec(`DROP TRIGGER IF EXISTS wait_repair_insert ON execution_repairs;DROP FUNCTION IF EXISTS wait_repair_insert();DROP SEQUENCE IF EXISTS repair_wait_seen`)
	})
	if _, e = f.Admin.Pool.Exec(`UPDATE model_budgets SET expires_at=clock_timestamp()+interval '1 second' WHERE work_item_uuid=$1`, f.Command.WorkItemID); e != nil {
		t.Fatal(e)
	}
	if sent, e := repairflow.Handoff(context.Background(), f.Verifier, repairLoad(f, "verifier")); e != nil || !sent {
		t.Fatal("short handoff", sent, e)
	}
	if _, e = relayRepair(t, f, "repair-candidate", repairLoad(f, "orchestrator")); e == nil {
		t.Fatal("expired derivation succeeded")
	}
	var called bool
	if e = f.Admin.Pool.QueryRow(`SELECT is_called FROM repair_wait_seen`).Scan(&called); e != nil || !called {
		t.Fatal("post-insert delay not exercised", e)
	}
	var children, allowances, commands int
	if e = f.Admin.Pool.QueryRow(`SELECT (SELECT count(*) FROM execution_repairs),(SELECT count(*) FROM repair_model_budgets),(SELECT count(*) FROM authorized_outbox WHERE operation='execute-work')`).Scan(&children, &allowances, &commands); e != nil || children != 0 || allowances != 0 || commands != 0 {
		t.Fatal("partial derivation after expiry", children, allowances, commands, e)
	}
}

func TestConnectedRepairRejectsChangedEvidenceBeforeDerivation(t *testing.T) {
	for _, mutation := range []string{
		`UPDATE work_items SET status='blocked'`,
		`UPDATE intake_heads SET generation=generation+1`,
		`UPDATE review_feedback SET digest=repeat('a',64)`,
		`UPDATE model_allocations SET reserved_calls=max_calls`,
		`UPDATE execution_requests SET stop_reason='uncertain'`,
	} {
		t.Run(mutation, func(t *testing.T) {
			f := execution.NewRepairFixtureForTest(t, true)
			if sent, e := repairflow.Handoff(context.Background(), f.Verifier, repairLoad(f, "verifier")); e != nil || !sent {
				t.Fatal(sent, e)
			}
			if _, e := f.Admin.Pool.Exec(mutation); e != nil {
				t.Fatal(e)
			}
			if _, e := relayRepair(t, f, "repair-candidate", repairLoad(f, "orchestrator")); e == nil {
				t.Fatal("changed evidence accepted")
			}
			var n int
			if e := f.Admin.Pool.QueryRow(`SELECT count(*) FROM execution_repairs`).Scan(&n); e != nil || n != 0 {
				t.Fatal("partial child", n, e)
			}
		})
	}
}

func TestConnectedRepairRequiresOriginalOptIn(t *testing.T) {
	f := execution.NewRepairFixtureForTest(t, false)
	if sent, e := repairflow.Handoff(context.Background(), f.Verifier, repairLoad(f, "verifier")); e != nil || sent {
		t.Fatal(sent, e)
	}
	var outcome string
	if e := f.Admin.Pool.QueryRow(`SELECT outcome FROM review_repair_handoffs WHERE review_id=$1`, f.Review.ID).Scan(&outcome); e != nil || outcome != "not-authorized" {
		t.Fatal(outcome, e)
	}
	if duplicate, e := relayRepair(t, f, "stop-rejected-work", nil); e != nil || duplicate {
		t.Fatal("stop rejected work", duplicate, e)
	}
	if duplicate, e := relayRepair(t, f, "stop-rejected-work", nil); e != nil || !duplicate {
		t.Fatal("duplicate stop fact", duplicate, e)
	}
	var state string
	if e := f.Admin.Pool.QueryRow(`SELECT status FROM work_items WHERE uuid=$1`, f.Command.WorkItemID).Scan(&state); e != nil || state != "failed" {
		t.Fatal("rejected work not closed", state, e)
	}
}

func TestConnectedRepairTerminalPreservesCompletedAndSupersededWork(t *testing.T) {
	for _, state := range []string{"blocked", "completed", "failed"} {
		t.Run(state, func(t *testing.T) {
			f := execution.NewRepairFixtureForTest(t, false)
			if _, e := repairflow.Handoff(context.Background(), f.Verifier, repairLoad(f, "verifier")); e != nil {
				t.Fatal(e)
			}
			if _, e := f.Admin.Pool.Exec(`UPDATE work_items SET status=$1 WHERE uuid=$2`, state, f.Command.WorkItemID); e != nil {
				t.Fatal(e)
			}
			if _, e := relayRepair(t, f, "stop-rejected-work", nil); e != nil {
				t.Fatal(e)
			}
			var after string
			if e := f.Admin.Pool.QueryRow(`SELECT status FROM work_items WHERE uuid=$1`, f.Command.WorkItemID).Scan(&after); e != nil || after != state {
				t.Fatal("terminal state overwritten", after, e)
			}
		})
	}
}

func TestConnectedRepairTerminalCommitAcknowledgementLoss(t *testing.T) {
	f := execution.NewRepairFixtureForTest(t, false)
	if _, e := repairflow.Handoff(context.Background(), f.Verifier, repairLoad(f, "verifier")); e != nil {
		t.Fatal(e)
	}
	fault, dropped := execution.RepairCommitFaultForTest(t, "orchestrator")
	faultFixture := f
	faultFixture.Orchestrator = fault
	if _, e := relayRepair(t, faultFixture, "stop-rejected-work", nil); e == nil {
		t.Fatal("lost terminal commit reported success")
	}
	select {
	case <-dropped:
	default:
		t.Fatal("terminal server reply not dropped")
	}
	if duplicate, e := relayRepair(t, f, "stop-rejected-work", nil); e != nil || !duplicate {
		t.Fatal("terminal replay", duplicate, e)
	}
	var state string
	if e := f.Admin.Pool.QueryRow(`SELECT status FROM work_items WHERE uuid=$1`, f.Command.WorkItemID).Scan(&state); e != nil || state != "failed" {
		t.Fatal(state, e)
	}
}

func TestConnectedRepairQueuedExpiryRetiresOnlyChild(t *testing.T) {
	t.Run("direct-sweeps", func(t *testing.T) { connectedRepairQueuedExpiry(t, "") })
	t.Run("gateway-unavailable", func(t *testing.T) { connectedRepairQueuedExpiry(t, "gateway") })
	// A missing sandbox class refuses new work but never blocks expiry.
	t.Run("sandbox-runtime-class-missing", func(t *testing.T) { connectedRepairQueuedExpiry(t, "sandbox") })
}
func connectedRepairQueuedExpiry(t *testing.T, unavailable string) {
	f := execution.NewRepairFixtureForTest(t, true)
	if _, e := f.Admin.Pool.Exec(`UPDATE model_budgets SET expires_at=clock_timestamp()+interval '2 seconds' WHERE work_item_uuid=$1`, f.Command.WorkItemID); e != nil {
		t.Fatal(e)
	}
	if sent, e := repairflow.Handoff(context.Background(), f.Verifier, repairLoad(f, "verifier")); e != nil || !sent {
		t.Fatal(sent, e)
	}
	if _, e := relayRepair(t, f, "repair-candidate", repairLoad(f, "orchestrator")); e != nil {
		t.Fatal(e)
	}
	if _, e := relayRepair(t, f, "execute-work", nil); e != nil {
		t.Fatal(e)
	}
	var child, original string
	var expires time.Time
	if e := f.Admin.Pool.QueryRow(`SELECT child_request_id,expires_at FROM execution_repairs`).Scan(&child, &expires); e != nil {
		t.Fatal(e)
	}
	if e := f.Admin.Pool.QueryRow(`SELECT row_to_json(b)::text FROM model_budgets b`).Scan(&original); e != nil {
		t.Fatal(e)
	}
	if d := time.Until(expires) + 20*time.Millisecond; d > 0 {
		time.Sleep(d)
	}
	ctx := context.Background()
	if unavailable != "" {
		pollUnavailableGateway(t, f, unavailable == "sandbox")
	} else {
		if e := f.Worker.ExpireQueuedExecutions(ctx); e != nil {
			t.Fatal(e)
		}
		if e := f.Worker.ExpireExecutionClaims(ctx); e != nil {
			t.Fatal(e)
		}
	}
	var state, reason, work, budget, after string
	var effects int
	if e := f.Admin.Pool.QueryRow(`SELECT e.state,e.stop_reason,w.status,b.state,(SELECT count(*) FROM execution_lifecycles WHERE request_id=$1) FROM execution_requests e JOIN work_items w ON w.uuid=e.work_item_uuid JOIN repair_model_budgets b ON b.work_item_uuid=w.uuid WHERE e.id=$1`, child).Scan(&state, &reason, &work, &budget, &effects); e != nil || state != "stopped" || reason != "expired" || work != "failed" || budget != "stopped" || effects != 0 {
		t.Fatal("queued child not safely retired", state, reason, work, budget, effects, e)
	}
	if e := f.Admin.Pool.QueryRow(`SELECT row_to_json(b)::text FROM model_budgets b`).Scan(&after); e != nil || after != original {
		t.Fatal("queued child recovery changed parent", e)
	}
}

func TestConnectedRepairTerminalFactSurvivesOriginalExpiry(t *testing.T) {
	f := execution.NewRepairFixtureForTest(t, false, 5*time.Second)
	if _, e := repairflow.Handoff(context.Background(), f.Verifier, repairLoad(f, "verifier")); e != nil {
		t.Fatal(e)
	}
	if wait := time.Until(time.UnixMilli(f.Command.ExpiresAt)) + 20*time.Millisecond; wait > 0 {
		time.Sleep(wait)
	}
	if _, e := relayRepair(t, f, "stop-rejected-work", nil); e != nil {
		t.Fatal("late rejection fact", e)
	}
	var state string
	if e := f.Admin.Pool.QueryRow(`SELECT status FROM work_items WHERE uuid=$1`, f.Command.WorkItemID).Scan(&state); e != nil || state != "failed" {
		t.Fatal(state, e)
	}
}
func TestConnectedRepairTerminalPreservesNewIssueHead(t *testing.T) {
	f := execution.NewRepairFixtureForTest(t, false)
	if _, e := repairflow.Handoff(context.Background(), f.Verifier, repairLoad(f, "verifier")); e != nil {
		t.Fatal(e)
	}
	if _, e := f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
		t.Fatal(e)
	}
	if _, e := relayRepair(t, f, "stop-rejected-work", nil); e != nil {
		t.Fatal(e)
	}
	var state string
	if e := f.Admin.Pool.QueryRow(`SELECT status FROM work_items WHERE uuid=$1`, f.Command.WorkItemID).Scan(&state); e != nil || state != "running" {
		t.Fatal("superseded work changed", state, e)
	}
}
