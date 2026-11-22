package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/reviewresult"
)

// ReviewFeedback is private, untrusted model prose bound to historical facts.
// It is never a command, a renewed claim, or publication/repair authorization.
type ReviewFeedback struct {
	Version             string `json:"version"`
	ReviewID            string `json:"reviewID"`
	VerificationID      string `json:"verificationID"`
	CandidateID         string `json:"candidateID"`
	WorkItemID          string `json:"workItemID"`
	BehaviorSHA256      string `json:"behaviorSHA256"`
	ProfileSHA256       string `json:"profileSHA256"`
	ConfigurationSHA256 string `json:"configurationSHA256"`
	AssignmentSHA256    string `json:"assignmentSHA256"`
	CandidateTreeSHA256 string `json:"candidateTreeSHA256"`
	ModelEvidenceSHA256 string `json:"modelEvidenceSHA256"`
	FinalSHA256         string `json:"finalSHA256"`
	Verdict             string `json:"verdict"`
	FinalMessage        string `json:"finalMessage"`
	Iterations          int    `json:"iterations"`
	ReadCalls           int    `json:"readCalls"`
}

type ReviewFeedbackRecord struct {
	Decision   ReviewFeedback
	Digest     string
	RecordedAt time.Time
}

// reviewFeedbackEvidence reconstructs bindings from the original journal.
// Both the live writer and historical reader use it; it grants no authority.
func reviewFeedbackEvidence(ctx context.Context, q Querier, id string, summary reviewresult.Result) (ReviewFeedback, time.Time, time.Time, error) {
	f := ReviewFeedback{Version: "ai-sdlc.review-feedback/v1", ReviewID: id, Verdict: summary.Verdict, FinalMessage: summary.FinalMessage, Iterations: summary.Iterations, ReadCalls: summary.ReadCalls}
	deny := func() (ReviewFeedback, time.Time, time.Time, error) {
		return ReviewFeedback{}, time.Time{}, time.Time{}, ErrExecution
	}
	if summary.Verdict != "request_changes" {
		return deny()
	}
	var behavior []byte
	var expires, until, created time.Time
	var calls int
	if q.QueryRowContext(ctx, `SELECT r.verification_id,r.candidate_id,r.work_item_uuid,r.behavior_digest,r.profile_digest,r.configuration_digest,r.assignment_digest,c.candidate_tree_sha256,v.proof,LEAST(r.expires_at,v.expires_at),r.claim_until,r.created_at,r.reserved_calls
 FROM review_runs r JOIN verification_requests v ON v.id=r.verification_id AND v.candidate_id=r.candidate_id JOIN execution_candidates c ON c.request_id=r.candidate_id JOIN execution_requests e ON e.id=c.request_id AND e.work_item_uuid=r.work_item_uuid
 WHERE r.id=$1 AND r.cleanup_state='complete' AND v.state='passed' AND v.cleanup_state='complete'`, id).Scan(&f.VerificationID, &f.CandidateID, &f.WorkItemID, &f.BehaviorSHA256, &f.ProfileSHA256, &f.ConfigurationSHA256, &f.AssignmentSHA256, &f.CandidateTreeSHA256, &behavior, &expires, &until, &created, &calls) != nil || authority.Digest(behavior) != f.BehaviorSHA256 || ReviewID(f.VerificationID, f.BehaviorSHA256, f.ProfileSHA256, f.ConfigurationSHA256, f.AssignmentSHA256) != id {
		return deny()
	}
	if until.Before(expires) {
		expires = until
	}
	var effects int
	var latest time.Time
	if q.QueryRowContext(ctx, `SELECT count(*),max(completed_at) FROM review_effects WHERE run_id=$1 AND resource_uid IS NOT NULL AND completed_at IS NOT NULL AND not_sent_at IS NULL`, id).Scan(&effects, &latest) != nil || effects != 5 {
		return deny()
	}
	if latest.After(created) {
		created = latest
	}
	var imported, exported, pod string
	if q.QueryRowContext(ctx, `SELECT a.digest,b.digest,a.resource_uid FROM review_effects a JOIN review_effects b ON b.run_id=a.run_id JOIN review_effects start ON start.run_id=a.run_id AND start.kind='start' JOIN review_effects p ON p.run_id=a.run_id AND p.kind='pod' WHERE a.run_id=$1 AND a.kind='import' AND b.kind='export' AND a.resource_uid=b.resource_uid AND a.resource_uid=start.resource_uid AND a.resource_uid=p.resource_uid`, id).Scan(&imported, &exported, &pod) != nil || imported != exported || imported != f.CandidateTreeSHA256 {
		return deny()
	}
	rows, err := q.QueryContext(ctx, `SELECT sequence,request_digest,COALESCE(response_digest,''),input_bytes,COALESCE(output_bytes,0),observation,state,handed_off_at,pod_uid FROM review_model_calls WHERE run_id=$1 ORDER BY sequence LIMIT 9`, id)
	if err != nil {
		return deny()
	}
	evidence := []ReviewModelEvidence{}
	observations := []reviewresult.Observation{}
	for rows.Next() {
		var v ReviewModelEvidence
		var state, observedPod string
		var handed time.Time
		var o reviewresult.Observation
		if rows.Scan(&v.Sequence, &v.RequestDigest, &v.ResponseDigest, &v.InputBytes, &v.OutputBytes, &v.Observation, &state, &handed, &observedPod) != nil || state != "handed-off" || observedPod != pod || v.Sequence != len(evidence)+1 || v.InputBytes < 1 || v.OutputBytes < 1 || !modelDigest.MatchString(v.RequestDigest) || !modelDigest.MatchString(v.ResponseDigest) || authority.DecodeStrict(v.Observation, &o) != nil {
			rows.Close()
			return deny()
		}
		if handed.After(created) {
			created = handed
		}
		evidence = append(evidence, v)
		observations = append(observations, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || calls != len(evidence) || reviewresult.MatchObservations(summary, observations) != nil {
		return deny()
	}
	raw, _ := json.Marshal(evidence)
	f.ModelEvidenceSHA256 = authority.Digest(raw)
	f.FinalSHA256 = authority.Digest([]byte(summary.FinalMessage))
	return f, created, expires, nil
}

// RejectReviewWithFeedback atomically records the exact authenticated feedback
// and closes the original live claim. A failed/unknown commit cannot be retried
// using that claim; the historical reader may observe a committed fact instead.
func (db *DB) RejectReviewWithFeedback(ctx context.Context, c ReviewClaim, summary reviewresult.Result) error {
	defer c.revoke()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		if lockReview(ctx, tx, c) != nil {
			return ErrExecution
		}
		f, earliest, expires, err := reviewFeedbackEvidence(ctx, tx, c.ID, summary)
		if err != nil {
			return ErrExecution
		}
		raw, err := json.Marshal(f)
		if err != nil || len(raw) > 32768 {
			return ErrExecution
		}
		if checkReviewTime(ctx, tx, c) != nil {
			return ErrExecution
		}
		var at time.Time
		if tx.QueryRowContext(ctx, `INSERT INTO review_feedback(review_id,decision,digest) VALUES($1,$2,$3) RETURNING recorded_at`, c.ID, raw, authority.Digest(raw)).Scan(&at) != nil || at.Before(earliest) || !at.Before(expires) {
			return ErrExecution
		}
		if _, err = tx.ExecContext(ctx, `UPDATE review_runs SET state='failed',reason='request-changes' WHERE id=$1`, c.ID); err != nil {
			return ErrExecution
		}
		return checkReviewTime(ctx, tx, c)
	})
}

// ReadReviewFeedback authenticates an already committed rejection without
// reopening its claim. Old rejections lacking feedback remain unavailable.
func (db *DB) ReadReviewFeedback(ctx context.Context, id string) (ReviewFeedbackRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var result ReviewFeedbackRecord
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "verifier") != nil {
			return ErrExecution
		}
		var err error
		result, err = readReviewFeedback(ctx, tx, id)
		return err
	})
	if err != nil {
		return ReviewFeedbackRecord{}, ErrExecution
	}
	return result, nil
}

// ReadRepairFeedback repeats the complete historical proof inside the
// orchestrator's derivation transaction. It grants no review mutation right.
func ReadRepairFeedback(ctx context.Context, tx *sql.Tx, id string) (ReviewFeedbackRecord, error) {
	if tx == nil || AssertComponent(ctx, tx, "orchestrator") != nil {
		return ReviewFeedbackRecord{}, ErrExecution
	}
	return readReviewFeedback(ctx, tx, id)
}

func readReviewFeedback(ctx context.Context, q Querier, id string) (ReviewFeedbackRecord, error) {
	var result ReviewFeedbackRecord
	var raw []byte
	if q.QueryRowContext(ctx, `SELECT f.decision,f.digest,f.recorded_at FROM review_feedback f JOIN review_runs r ON r.id=f.review_id WHERE f.review_id=$1 AND r.state='failed' AND r.reason='request-changes' AND r.cleanup_state='complete'`, id).Scan(&raw, &result.Digest, &result.RecordedAt) != nil || len(raw) < 1 || len(raw) > 32768 || authority.Digest(raw) != result.Digest || authority.DecodeStrict(raw, &result.Decision) != nil {
		return ReviewFeedbackRecord{}, ErrExecution
	}
	f := result.Decision
	expected, earliest, expires, err := reviewFeedbackEvidence(ctx, q, id, reviewresult.Result{Verdict: f.Verdict, FinalMessage: f.FinalMessage, Iterations: f.Iterations, ReadCalls: f.ReadCalls})
	canonical, _ := json.Marshal(expected)
	if err != nil || !bytes.Equal(canonical, raw) || result.RecordedAt.Before(earliest) || !result.RecordedAt.Before(expires) || result.RecordedAt.After(time.Now()) {
		return ReviewFeedbackRecord{}, ErrExecution
	}
	return result, nil
}
