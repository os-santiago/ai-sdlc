package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// NextCandidateHandoff is a bounded queue hint, never verification authority.
func (db *DB) NextCandidateHandoff(ctx context.Context, revision string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "worker") != nil {
		return "", ErrExecution
	}
	var id string
	err := db.Pool.QueryRowContext(ctx, `SELECT c.request_id FROM execution_candidates c JOIN execution_requests r ON r.id=c.request_id JOIN execution_lifecycles l ON l.request_id=c.request_id WHERE r.authority_revision=$1 AND r.expires_at>clock_timestamp() AND l.phase='candidate-captured' AND l.cleanup_state='complete' AND NOT EXISTS(SELECT 1 FROM verification_handoffs h WHERE h.candidate_id=c.request_id) ORDER BY c.created_at,c.request_id LIMIT 1`, revision).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", ErrExecution
	}
	return id, nil
}

// RecordCandidateHandoff belongs to the same transaction as signed outbox append.
// Any existing disposition is terminal; a lost acknowledgement is never adopted.
func RecordCandidateHandoff(ctx context.Context, tx *sql.Tx, candidate, command, digest string) error {
	if tx == nil || AssertComponent(ctx, tx, "worker") != nil {
		return ErrExecution
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO verification_handoffs(candidate_id,command_id,payload_digest,state) SELECT c.request_id,$2,$3,'sent' FROM execution_candidates c JOIN execution_lifecycles l ON l.request_id=c.request_id JOIN execution_requests r ON r.id=c.request_id WHERE c.request_id=$1 AND l.phase='candidate-captured' AND l.cleanup_state='complete' AND r.expires_at>clock_timestamp()`, candidate, command, digest)
	if err != nil {
		return ErrExecution
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return ErrExecution
	}
	return nil
}
func (db *DB) RejectCandidateHandoff(ctx context.Context, id, reason string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if (reason != "unsupported-recipe" && reason != "revoked") || AssertComponent(ctx, db.Pool, "worker") != nil {
		return ErrExecution
	}
	_, err := db.Pool.ExecContext(ctx, `INSERT INTO verification_handoffs(candidate_id,state,reason) VALUES($1,'rejected',$2) ON CONFLICT(candidate_id) DO NOTHING`, id, reason)
	if err != nil {
		return ErrExecution
	}
	return nil
}
