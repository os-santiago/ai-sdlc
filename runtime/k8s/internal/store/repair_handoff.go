package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

func (db *DB) NextRepairFeedback(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return "", ErrExecution
	}
	var id string
	err := db.Pool.QueryRowContext(ctx, `SELECT f.review_id FROM review_feedback f JOIN review_runs r ON r.id=f.review_id
 WHERE r.state='failed' AND r.reason='request-changes' AND r.cleanup_state='complete'
 AND NOT EXISTS(SELECT 1 FROM review_repair_handoffs h WHERE h.review_id=f.review_id)
 ORDER BY f.recorded_at,f.review_id LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", observationError(err)
	}
	return id, nil
}

// Terminal outcomes contain no command and cannot reenter the repair queue.
func RecordRepairHandoff(ctx context.Context, tx *sql.Tx, review, work, feedbackDigest, outcome string, payload []byte) error {
	if AssertComponent(ctx, tx, "verifier") != nil || !modelDigest.MatchString(feedbackDigest) {
		return ErrExecution
	}
	var digest any
	var body any
	if outcome == "forwarded" {
		if len(payload) < 1 || len(payload) > 16384 {
			return ErrExecution
		}
		digest, body = authority.Digest(payload), payload
	} else {
		switch outcome {
		case "not-authorized", "expired", "exhausted", "stale", "terminal-rejection":
		default:
			return ErrExecution
		}
		if len(payload) != 0 {
			return ErrExecution
		}
	}
	r, e := tx.ExecContext(ctx, `INSERT INTO review_repair_handoffs(review_id,work_item_uuid,outcome,payload,payload_digest)
 SELECT $1,$2,$4,$5,$6 WHERE EXISTS(SELECT 1 FROM review_feedback f JOIN review_runs r ON r.id=f.review_id
 WHERE f.review_id=$1 AND f.digest=$3 AND r.work_item_uuid=$2 AND r.state='failed' AND r.reason='request-changes' AND r.cleanup_state='complete')`, review, work, feedbackDigest, outcome, body, digest)
	if e != nil {
		return ErrExecution
	}
	n, e := r.RowsAffected()
	if e != nil || n != 1 {
		return ErrExecution
	}
	return nil
}

func ReadRepairBehavior(ctx context.Context, tx *sql.Tx, id string) ([]byte, error) {
	if AssertComponent(ctx, tx, "orchestrator") != nil {
		return nil, ErrExecution
	}
	var raw []byte
	if tx.QueryRowContext(ctx, `SELECT proof FROM verification_requests WHERE id=$1 AND state='passed' AND cleanup_state='complete'`, id).Scan(&raw) != nil || len(raw) < 1 || len(raw) > 65536 {
		return nil, ErrExecution
	}
	return raw, nil
}
