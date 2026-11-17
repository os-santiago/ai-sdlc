package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repair"
)

// NextRejectedStop also covers a forwarded repair whose original deadline
// expired before the child reached its inbox. No execution is manufactured.
func (db *DB) NextRejectedStop(ctx context.Context) (review, reason, child string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return "", "", "", ErrExecution
	}
	err = db.Pool.QueryRowContext(ctx, `SELECT h.review_id,CASE WHEN h.outcome='forwarded' THEN 'expired' ELSE h.outcome END,COALESCE(r.child_request_id,'')
 FROM review_repair_handoffs h JOIN model_budgets b ON b.work_item_uuid=h.work_item_uuid
 LEFT JOIN execution_repairs r ON r.work_item_uuid=h.work_item_uuid
 WHERE NOT EXISTS(SELECT 1 FROM review_repair_stops s WHERE s.review_id=h.review_id)
 AND (h.outcome<>'forwarded' OR (b.expires_at<=clock_timestamp() AND NOT EXISTS(SELECT 1 FROM execution_requests e WHERE e.id=r.child_request_id)))
 ORDER BY h.created_at,h.review_id LIMIT 1`).Scan(&review, &reason, &child)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", nil
	}
	if err != nil {
		return "", "", "", ErrObservationUnavailable
	}
	if reason != "expired" {
		child = ""
	}
	return
}

func RecordRejectedStop(ctx context.Context, tx *sql.Tx, s repair.Stop, raw []byte) error {
	if AssertComponent(ctx, tx, "verifier") != nil || !repair.TerminalReason(s.Reason) {
		return ErrExecution
	}
	canonical, _ := json.Marshal(s)
	if !bytes.Equal(raw, canonical) {
		return ErrExecution
	}
	var outcome string
	if tx.QueryRowContext(ctx, `SELECT h.outcome FROM review_repair_handoffs h JOIN review_feedback f ON f.review_id=h.review_id
 WHERE h.review_id=$1 AND h.work_item_uuid=$2 AND f.digest=$3`, s.ReviewID, s.WorkItemID, s.FeedbackSHA256).Scan(&outcome) != nil {
		return ErrExecution
	}
	if outcome != s.Reason {
		if outcome != "forwarded" || s.Reason != "expired" {
			return ErrExecution
		}
		var eligible bool
		if tx.QueryRowContext(ctx, `SELECT b.expires_at<=clock_timestamp() AND COALESCE(r.child_request_id,'')=$2
 AND NOT EXISTS(SELECT 1 FROM execution_requests e WHERE e.id=r.child_request_id)
 FROM model_budgets b LEFT JOIN execution_repairs r ON r.work_item_uuid=b.work_item_uuid WHERE b.work_item_uuid=$1`, s.WorkItemID, s.UnstartedChildID).Scan(&eligible) != nil || !eligible {
			return ErrExecution
		}
	}
	_, e := tx.ExecContext(ctx, `INSERT INTO review_repair_stops(review_id,work_item_uuid,payload,payload_digest) VALUES($1,$2,$3,$4)`, s.ReviewID, s.WorkItemID, raw, authority.Digest(raw))
	if e != nil {
		return ErrExecution
	}
	return nil
}

// CloseRejectedWork accepts only a committed verifier fact and already retired
// resources. It cannot cancel an active child, unblock work or touch budgets.
func CloseRejectedWork(ctx context.Context, tx *sql.Tx, s repair.Stop, raw []byte, candidate string, issue int, generation int64) (string, error) {
	if AssertComponent(ctx, tx, "orchestrator") != nil {
		return "", ErrExecution
	}
	var stored []byte
	if tx.QueryRowContext(ctx, `SELECT payload FROM review_repair_stops WHERE review_id=$1 AND work_item_uuid=$2 AND payload_digest=$3`, s.ReviewID, s.WorkItemID, authority.Digest(raw)).Scan(&stored) != nil || !bytes.Equal(stored, raw) {
		return "", ErrExecution
	}
	if _, e := tx.ExecContext(ctx, `SELECT public.lock_repair_parent_budget($1::uuid)`, s.WorkItemID); e != nil {
		return "", ErrExecution
	}
	var status string
	if tx.QueryRowContext(ctx, `SELECT status FROM work_items WHERE uuid=$1 FOR UPDATE`, s.WorkItemID).Scan(&status) != nil {
		return "", ErrExecution
	}
	if status != "running" {
		return "preserved", nil
	}
	var current bool
	if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM intake_heads WHERE repository=$1 AND issue_number=$2 AND generation=$3)`, s.Repository, issue, generation).Scan(&current) != nil {
		return "", ErrExecution
	}
	if !current {
		return "superseded", nil
	}
	var safe bool
	if tx.QueryRowContext(ctx, `SELECT
 NOT EXISTS(SELECT 1 FROM execution_requests WHERE work_item_uuid=$1 AND state IN ('queued','claimed'))
 AND NOT EXISTS(SELECT 1 FROM execution_lifecycles l JOIN execution_requests e ON e.id=l.request_id WHERE e.work_item_uuid=$1 AND l.cleanup_state<>'complete')
 AND NOT EXISTS(SELECT 1 FROM verification_requests v JOIN execution_requests e ON e.id=v.candidate_id WHERE e.work_item_uuid=$1 AND (v.state IN ('queued','claimed') OR v.cleanup_state<>'complete'))
 AND NOT EXISTS(SELECT 1 FROM review_runs WHERE work_item_uuid=$1 AND (state IN ('queued','claimed') OR cleanup_state<>'complete'))
 AND NOT EXISTS(SELECT 1 FROM publication_handoffs p JOIN review_runs r ON r.id=p.review_id WHERE r.work_item_uuid=$1)
 AND NOT EXISTS(SELECT 1 FROM execution_repairs r JOIN repair_model_budgets b USING(work_item_uuid) WHERE r.work_item_uuid=$1 AND NOT (
  (r.child_request_id=$2 AND b.state='stopped' AND b.stop_reason='candidate-captured' AND $3='')
  OR (r.parent_candidate_id=$2 AND r.child_request_id=$3 AND b.state='reserved' AND b.expires_at<=clock_timestamp()
   AND NOT EXISTS(SELECT 1 FROM execution_requests e WHERE e.id=r.child_request_id))))`, s.WorkItemID, candidate, s.UnstartedChildID).Scan(&safe) != nil || !safe {
		return "", ErrExecution
	}
	if _, e := tx.ExecContext(ctx, `UPDATE work_items SET status='failed',updated_at=clock_timestamp() WHERE uuid=$1 AND status='running'`, s.WorkItemID); e != nil {
		return "", ErrExecution
	}
	return "failed", nil
}

func CheckRejectedHead(ctx context.Context, tx *sql.Tx, repository string, issue int, generation int64) error {
	if AssertComponent(ctx, tx, "orchestrator") != nil {
		return ErrExecution
	}
	var current bool
	if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM intake_heads WHERE repository=$1 AND issue_number=$2 AND generation=$3)`, repository, issue, generation).Scan(&current) != nil || !current {
		return ErrExecution
	}
	return nil
}
