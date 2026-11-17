package execution

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/reviewresult"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func reviewFeedbackFixture(t *testing.T, ttl ...time.Duration) (*lifecycleFixture, *store.DB, store.ReviewRun, store.ReviewClaim, reviewresult.Result) {
	return reviewFeedbackFixtureConfigured(t, nil, ttl...)
}

func reviewFeedbackFixtureConfigured(t *testing.T, configure []func(*fixture), ttl ...time.Duration) (*lifecycleFixture, *store.DB, store.ReviewRun, store.ReviewClaim, reviewresult.Result) {
	t.Helper()
	f, db, r := reviewJournalFixtureConfigured(t, nil, configure)
	ctx := context.Background()
	{
		passwords := map[string]string{}
		for role := range f.Material.Identities {
			passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
		}
		if e := f.Admin.ProvisionRolesForOptions(ctx, passwords, "verification", true); e != nil {
			t.Fatal(e)
		}
	}
	if len(ttl) > 0 {
		if _, err := f.Admin.Pool.Exec(`UPDATE review_runs SET expires_at=clock_timestamp()+$2::interval WHERE id=$1`, r.ID, ttl[0].String()); err != nil {
			t.Fatal(err)
		}
	}
	c, result := reviewFeedbackEffects(t, f.Admin, db, r)
	return f, db, r, c, result
}

func reviewFeedbackEffects(t *testing.T, admin, db *store.DB, r store.ReviewRun) (store.ReviewClaim, reviewresult.Result) {
	t.Helper()
	ctx := context.Background()
	c := claimReview(t, db, r)
	var tree string
	if admin.Pool.QueryRow(`SELECT candidate_tree_sha256 FROM execution_candidates WHERE request_id=$1`, r.CandidateID).Scan(&tree) != nil {
		t.Fatal("candidate tree")
	}
	pod := store.NewID()
	for _, kind := range []string{"config", "pod", "import", "start"} {
		e, err := db.BeginReviewEffect(ctx, c, kind)
		if err != nil {
			t.Fatal(err)
		}
		uid, digest := pod, ""
		if kind == "config" {
			uid = store.NewID()
		}
		if kind == "import" {
			digest = tree
		}
		if db.AcknowledgeReviewEffect(ctx, e, uid, digest) != nil {
			t.Fatal("effect")
		}
	}
	for sequence := 1; sequence <= 2; sequence++ {
		call, e := db.BeginReviewModelCall(ctx, c, pod, sequence, []byte(`{"model":"review-model","messages":[{"role":"user","content":"review"}]}`))
		if e != nil {
			t.Fatal(e)
		}
		message := map[string]any{"role": "assistant", "content": "Add a boundary regression test.\nVERDICT: request_changes"}
		finish := "stop"
		if sequence == 1 {
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"id": "read-1", "type": "function", "function": map[string]string{"name": "read_file", "arguments": `{"path":"src/a.js"}`}}}
			finish = "tool_calls"
		}
		response, _ := json.Marshal(map[string]any{"model": "review-model", "choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}}})
		observed, e := modelbroker.ObserveReviewResponse(response, "review-model")
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(observed)
		if db.CompleteReviewModelCall(ctx, call, response, raw) != nil || db.AcknowledgeReviewModelCall(ctx, call, authority.Digest(response)) != nil {
			t.Fatal("model receipt")
		}
	}

	effect, e := db.BeginReviewEffect(ctx, c, "export")
	if e != nil || db.AcknowledgeReviewEffect(ctx, effect, pod, tree) != nil {
		t.Fatal("export")
	}
	if db.ConfirmReviewCleanup(ctx, c) != nil {
		t.Fatal("cleanup")
	}
	return c, reviewresult.Result{Verdict: "request_changes", FinalMessage: "Add a boundary regression test.\nVERDICT: request_changes", Iterations: 2, ReadCalls: 1}
}

func TestReviewFeedbackExactPrivateHistoricalRecord(t *testing.T) {
	f, db, r, c, summary := reviewFeedbackFixture(t)
	ctx := context.Background()
	if db.RejectReviewWithFeedback(ctx, c, summary) != nil {
		t.Fatal("exact feedback rejected")
	}
	record, err := db.ReadReviewFeedback(ctx, r.ID)
	if err != nil || record.Decision.FinalMessage != summary.FinalMessage || record.Decision.CandidateID != r.CandidateID || record.Decision.VerificationID != r.VerificationID || record.Decision.FinalSHA256 != authority.Digest([]byte(summary.FinalMessage)) {
		t.Fatal("historical feedback unavailable or changed", err)
	}
	if db.RejectReviewWithFeedback(ctx, c, summary) == nil {
		t.Fatal("rejection replay")
	}
	if _, err = f.db.ReadReviewFeedback(ctx, r.ID); err == nil {
		t.Fatal("generator read private feedback")
	}
	for _, sql := range []string{`UPDATE review_feedback SET digest=repeat('a',64)`, `DELETE FROM review_feedback`, `TRUNCATE review_feedback`} {
		if _, err = db.Pool.Exec(sql); err == nil {
			t.Fatal("feedback mutation permitted")
		}
	}
	if next, err := db.NextVerifiedForReview(ctx); err != nil || next != "" {
		t.Fatal("rejected review redispatched")
	}
	var state, reason string
	if f.Admin.Pool.QueryRow(`SELECT state,reason FROM review_runs WHERE id=$1`, r.ID).Scan(&state, &reason) != nil || state != "failed" || reason != "request-changes" {
		t.Fatal("decision not atomic")
	}
	if f.Count(t, `SELECT count(*) FROM publication_requests`) != 0 {
		t.Fatal("feedback created publication")
	}
	// Repeated historical reads neither change the record nor open a new claim.
	time.Sleep(5 * time.Millisecond)
	if after, err := db.ReadReviewFeedback(ctx, r.ID); err != nil || after.Digest != record.Digest {
		t.Fatal("historical reader changed record", err)
	}
}

func TestReviewFeedbackRejectsMismatchAndMissingCleanup(t *testing.T) {
	for _, fault := range []string{"message", "verdict", "iterations", "reads", "tree", "behavior", "cleanup", "claim", "other-role", "pod"} {
		t.Run(fault, func(t *testing.T) {
			f, db, r, c, s := reviewFeedbackFixture(t)
			ctx := context.Background()
			actor := db
			switch fault {
			case "message":
				s.FinalMessage = "Different finding.\nVERDICT: request_changes"
			case "verdict":
				s.Verdict = "approve"
				s.FinalMessage = "VERDICT: approve"
			case "iterations":
				s.Iterations++
			case "reads":
				s.ReadCalls++
			case "tree":
				if _, e := f.Admin.Pool.Exec(`UPDATE review_effects SET digest=repeat('e',64) WHERE run_id=$1 AND kind='export'`, r.ID); e != nil {
					t.Fatal(e)
				}
			case "behavior":
				if _, e := f.Admin.Pool.Exec(`UPDATE review_runs SET behavior_digest=repeat('e',64) WHERE id=$1`, r.ID); e != nil {
					t.Fatal(e)
				}
			case "cleanup":
				if _, e := f.Admin.Pool.Exec(`UPDATE review_runs SET cleanup_state='pending' WHERE id=$1`, r.ID); e != nil {
					t.Fatal(e)
				}
			case "pod":
				if _, e := f.Admin.Pool.Exec(`UPDATE review_model_calls SET pod_uid=$2 WHERE run_id=$1`, r.ID, store.NewID()); e != nil {
					t.Fatal(e)
				}
			case "claim":
				c.Token = store.NewID()
			case "other-role":
				actor = f.db
			}
			if actor.RejectReviewWithFeedback(ctx, c, s) == nil {
				t.Fatal("invalid feedback accepted")
			}
			if f.Count(t, `SELECT count(*) FROM review_feedback`) != 0 {
				t.Fatal("partial feedback persisted")
			}
			var state string
			if f.Admin.Pool.QueryRow(`SELECT state FROM review_runs WHERE id=$1`, r.ID).Scan(&state) != nil || state != "claimed" {
				t.Fatal("partial decision committed")
			}
		})
	}
}

func TestReviewFeedbackLostCommitAndInsertDenial(t *testing.T) {
	for _, fault := range []string{"lost-commit", "insert-denied"} {
		t.Run(fault, func(t *testing.T) {
			f, db, r, c, s := reviewFeedbackFixture(t)
			actor := db
			var dropped <-chan struct{}
			if fault == "lost-commit" {
				actor, dropped = executionCommitFault(t, 1, "verifier")
			} else {
				if _, e := f.Admin.Pool.Exec(`ALTER TABLE review_feedback ADD CONSTRAINT fixture_deny_feedback CHECK(false)`); e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() {
					_, _ = f.Admin.Pool.Exec(`ALTER TABLE review_feedback DROP CONSTRAINT IF EXISTS fixture_deny_feedback`)
				})
			}
			if actor.RejectReviewWithFeedback(context.Background(), c, s) == nil {
				t.Fatal("uncertain write reported success")
			}
			want := 0
			if fault == "lost-commit" {
				select {
				case <-dropped:
				default:
					t.Fatal("real COMMIT ACK not dropped")
				}
				want = 1
				record, e := db.ReadReviewFeedback(context.Background(), r.ID)
				if e != nil || record.Decision.FinalMessage != s.FinalMessage {
					t.Fatal("committed historical decision unavailable", e)
				}
			}
			if f.Count(t, `SELECT count(*) FROM review_feedback`) != want {
				t.Fatal("atomic record count")
			}
			if db.RejectReviewWithFeedback(context.Background(), c, s) == nil {
				t.Fatal("original claim replayed")
			}
		})
	}
}

func TestReviewFeedbackExpiryDuringInsertRollsBackDecision(t *testing.T) {
	f, db, r, c, s := reviewFeedbackFixture(t, 3*time.Second)
	ctx := context.Background()
	if _, e := f.Admin.Pool.Exec(`CREATE FUNCTION public.feedback_record_pause() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(81927832); RETURN NEW; END $$; CREATE TRIGGER feedback_record_pause AFTER INSERT ON review_feedback FOR EACH ROW EXECUTE FUNCTION public.feedback_record_pause()`); e != nil {
		t.Fatal(e)
	}
	lock, e := f.Admin.Pool.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Rollback()
	if _, e = lock.ExecContext(ctx, `SELECT pg_advisory_xact_lock(81927832)`); e != nil {
		t.Fatal(e)
	}
	expiry := c.Until
	if wait := time.Until(expiry.Add(-1200 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	finished := make(chan error, 1)
	go func() { finished <- db.RejectReviewWithFeedback(ctx, c, s) }()
	observed := false
	end := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(end) {
		var n int
		if f.Admin.Pool.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objid=81927832 AND NOT granted`).Scan(&n) == nil && n == 1 {
			observed = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !observed {
		t.Fatal("did not observe real INSERT wait")
	}
	time.Sleep(time.Until(expiry.Add(30 * time.Millisecond)))
	if e = lock.Rollback(); e != nil {
		t.Fatal(e)
	}
	select {
	case e = <-finished:
		if e == nil {
			t.Fatal("expired feedback committed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer unbounded")
	}
	var state string
	if f.Count(t, `SELECT count(*) FROM review_feedback`) != 0 || f.Admin.Pool.QueryRow(`SELECT state FROM review_runs WHERE id=$1`, r.ID).Scan(&state) != nil || state != "claimed" {
		t.Fatal("partial expired decision")
	}
}

func TestReviewFeedbackHistoricalReaderDoesNotRenewExpiry(t *testing.T) {
	_, db, r, c, s := reviewFeedbackFixture(t, 3*time.Second)
	ctx := context.Background()
	expiry := c.Until
	if db.RejectReviewWithFeedback(ctx, c, s) != nil {
		t.Fatal("initial feedback")
	}
	time.Sleep(time.Until(expiry.Add(30 * time.Millisecond)))
	if _, e := db.ReadReviewFeedback(ctx, r.ID); e != nil {
		t.Fatal("expired historical fact unavailable", e)
	}
	if db.RejectReviewWithFeedback(ctx, c, s) == nil {
		t.Fatal("historical read renewed claim")
	}
}

func TestReviewFeedbackHistoricalReaderRejectsSubstitution(t *testing.T) {
	f, db, r, c, s := reviewFeedbackFixture(t)
	ctx := context.Background()
	if db.RejectReviewWithFeedback(ctx, c, s) != nil {
		t.Fatal("initial feedback")
	}
	record, e := db.ReadReviewFeedback(ctx, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	original, _ := json.Marshal(record.Decision)
	for _, field := range []string{"candidate", "tree", "verification", "work", "behavior", "profile", "configuration", "assignment", "model", "message", "final", "iterations", "reads"} {
		changed := record.Decision
		switch field {
		case "candidate":
			changed.CandidateID = "execution-" + strings.Repeat("a", 64)
		case "tree":
			changed.CandidateTreeSHA256 = strings.Repeat("a", 64)
		case "verification":
			changed.VerificationID = "verification-" + strings.Repeat("a", 64)
		case "work":
			changed.WorkItemID = store.NewID()
		case "behavior":
			changed.BehaviorSHA256 = strings.Repeat("a", 64)
		case "profile":
			changed.ProfileSHA256 = strings.Repeat("a", 64)
		case "configuration":
			changed.ConfigurationSHA256 = strings.Repeat("a", 64)
		case "assignment":
			changed.AssignmentSHA256 = strings.Repeat("a", 64)
		case "model":
			changed.ModelEvidenceSHA256 = strings.Repeat("a", 64)
		case "message":
			changed.FinalMessage = "Another finding.\nVERDICT: request_changes"
			changed.FinalSHA256 = authority.Digest([]byte(changed.FinalMessage))
		case "final":
			changed.FinalSHA256 = strings.Repeat("a", 64)
		case "iterations":
			changed.Iterations++
		case "reads":
			changed.ReadCalls++
		}
		raw, _ := json.Marshal(changed)
		if _, e = f.Admin.Pool.Exec(`UPDATE review_feedback SET decision=$2,digest=$3 WHERE review_id=$1`, r.ID, raw, authority.Digest(raw)); e != nil {
			t.Fatal(e)
		}
		if _, e = db.ReadReviewFeedback(ctx, r.ID); e == nil {
			t.Fatal("substituted historical feedback accepted", field)
		}
	}
	if _, e = f.Admin.Pool.Exec(`UPDATE review_feedback SET decision=$2,digest=$3 WHERE review_id=$1`, r.ID, original, record.Digest); e != nil {
		t.Fatal(e)
	}
	if _, e = db.ReadReviewFeedback(ctx, r.ID); e != nil {
		t.Fatal("original history not readable", e)
	}
}

func TestReviewFeedbackLegacyRejectionRemainsUnavailable(t *testing.T) {
	f, db, r, c, _ := reviewFeedbackFixture(t)
	ctx := context.Background()
	if db.StopReview(ctx, c, "request-changes") != nil {
		t.Fatal("historical rejection")
	}
	if _, e := db.ReadReviewFeedback(ctx, r.ID); e == nil {
		t.Fatal("invented legacy feedback")
	}
	if f.Count(t, `SELECT count(*) FROM review_feedback`) != 0 {
		t.Fatal("history backfilled")
	}
}
