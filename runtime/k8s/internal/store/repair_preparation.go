package store

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repair"
)

// ReadRepairPreparation exposes private feedback only for an exact child
// committed by the orchestrator. It cannot discover or create another repair.
func (db *DB) ReadRepairPreparation(ctx context.Context, request string, payload []byte) (repair.Assignment, VerificationCandidate, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	deny := func() (repair.Assignment, VerificationCandidate, error) {
		return repair.Assignment{}, VerificationCandidate{}, ErrExecution
	}
	if AssertComponent(ctx, db.Pool, "worker") != nil {
		return deny()
	}
	var parent, review, digest string
	var child, feedback []byte
	var calls int
	if db.Pool.QueryRowContext(ctx, `SELECT r.parent_candidate_id,r.parent_review_id,r.child_payload,f.decision,f.digest,b.max_calls
 FROM execution_repairs r JOIN repair_model_budgets b ON b.work_item_uuid=r.work_item_uuid AND b.allocation_id=r.child_allocation_id
 JOIN review_repair_handoffs h ON h.review_id=r.parent_review_id AND h.work_item_uuid=r.work_item_uuid AND h.payload_digest=r.handoff_digest
 JOIN review_feedback f ON f.review_id=r.parent_review_id
 JOIN execution_requests original ON original.id=r.parent_candidate_id AND original.work_item_uuid=r.work_item_uuid
 JOIN work_items w ON w.uuid=r.work_item_uuid
 WHERE r.child_request_id=$1 AND r.expires_at>clock_timestamp() AND b.expires_at=r.expires_at AND h.outcome='forwarded'
 AND original.attempt=1 AND original.state='stopped' AND original.stop_reason='candidate-captured' AND w.status='running'
 AND (b.state IN ('reserved','active') OR (b.state='stopped' AND b.stop_reason='candidate-captured'))`, request).Scan(&parent, &review, &child, &feedback, &digest, &calls) != nil || !bytes.Equal(child, payload) || len(feedback) < 1 || len(feedback) > 32768 || authority.Digest(feedback) != digest {
		return deny()
	}
	var f ReviewFeedback
	if authority.DecodeStrict(feedback, &f) != nil {
		return deny()
	}
	canonical, _ := json.Marshal(f)
	if !bytes.Equal(canonical, feedback) || f.ReviewID != review || f.CandidateID != parent || f.Verdict != "request_changes" {
		return deny()
	}
	p, e := db.ReadCapturedCandidate(ctx, parent)
	if e != nil || p.WorkItem != f.WorkItemID || p.Record.CandidateTreeSHA256 != f.CandidateTreeSHA256 {
		return deny()
	}
	a := repair.Assignment{Reference: repair.Reference{ParentCandidateID: parent, ParentReviewID: review, VerificationID: f.VerificationID,
		FeedbackSHA256: digest, BehaviorSHA256: f.BehaviorSHA256, ParentTreeSHA256: f.CandidateTreeSHA256, OriginalTreeSHA256: p.Record.SourceTreeSHA256},
		Feedback: f.FinalMessage, FinalSHA256: f.FinalSHA256, RemainingCalls: calls}
	if !a.Valid() {
		return deny()
	}
	return a, p, nil
}
