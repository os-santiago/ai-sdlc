package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// FinishVerification stores behavior evidence only; it does not issue a merge,
// agent review, publication or release decision.
func (db *DB) FinishVerification(ctx context.Context, c VerificationClaim, proof []byte) error {
	return db.FinishVerificationWith(ctx, c, proof, []string{"baseline", "candidate", "config", "reference", "tests"})
}

// FinishVerificationWith stores behavior evidence when the request's effects
// are exactly the given kinds, every one acknowledged with a resource UID
// (the reference recipes create five resources, repository-tests/v1 four).
func (db *DB) FinishVerificationWith(ctx context.Context, c VerificationClaim, proof []byte, kinds []string) error {
	expected := map[string]bool{}
	for _, kind := range kinds {
		if verificationResourceName(c.ID, kind) == "" {
			return ErrExecution
		}
		expected[kind] = true
	}
	if len(proof) == 0 || len(proof) > 65536 || !json.Valid(proof) || len(expected) == 0 || len(expected) != len(kinds) {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		if lockVerification(ctx, tx, c) != nil {
			return ErrExecution
		}
		var clean bool
		var count int
		if tx.QueryRowContext(ctx, `SELECT cleanup_state='complete' FROM verification_requests WHERE id=$1`, c.ID).Scan(&clean) != nil || !clean {
			return ErrExecution
		}
		if tx.QueryRowContext(ctx, `SELECT count(*) FROM verification_effects WHERE request_id=$1`, c.ID).Scan(&count) != nil || count != len(expected) {
			return ErrExecution
		}
		rows, err := tx.QueryContext(ctx, `SELECT kind FROM verification_effects WHERE request_id=$1 AND resource_uid IS NOT NULL`, c.ID)
		if err != nil {
			return ErrExecution
		}
		acknowledged := 0
		for rows.Next() {
			var kind string
			if rows.Scan(&kind) != nil || !expected[kind] {
				rows.Close()
				return ErrExecution
			}
			acknowledged++
		}
		if rows.Close() != nil || rows.Err() != nil || acknowledged != len(expected) {
			return ErrExecution
		}
		r, err := tx.ExecContext(ctx, `UPDATE verification_requests SET state='passed',proof=$2 WHERE id=$1 AND state='claimed' AND claim_until>clock_timestamp() AND expires_at>clock_timestamp()`, c.ID, proof)
		if err != nil {
			return ErrExecution
		}
		n, err := r.RowsAffected()
		if err != nil || n != 1 {
			return ErrExecution
		}
		return nil
	})
}
func (db *DB) ExpireVerifications(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return ErrExecution
	}
	_, err := db.Pool.ExecContext(ctx, `WITH expired AS (SELECT id FROM verification_requests WHERE (state='queued' AND expires_at<=clock_timestamp()) OR (state='claimed' AND claim_until<=clock_timestamp()) ORDER BY created_at LIMIT 32 FOR UPDATE SKIP LOCKED) UPDATE verification_requests r SET cleanup_state=CASE WHEN r.state='queued' THEN 'complete' ELSE r.cleanup_state END,state='failed',reason='expired' FROM expired e WHERE r.id=e.id`)
	if err != nil {
		return ErrExecution
	}
	return nil
}

type VerificationCleanup struct {
	Claim   VerificationClaim
	Attempt int
}

func (db *DB) NextVerificationCleanup(ctx context.Context) (VerificationCleanup, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var c VerificationCleanup
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "verifier") != nil {
			return ErrExecution
		}
		err := tx.QueryRowContext(ctx, `SELECT id,claim_token,claim_until FROM verification_requests WHERE state='failed' AND cleanup_state='pending' AND claim_token IS NOT NULL AND cleanup_attempts<3 AND cleanup_next_at<=clock_timestamp() ORDER BY created_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&c.Claim.ID, &c.Claim.Token, &c.Claim.Until)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return ErrExecution
		}
		if tx.QueryRowContext(ctx, `UPDATE verification_requests SET cleanup_attempts=cleanup_attempts+1,cleanup_next_at=clock_timestamp()+interval '30 seconds' WHERE id=$1 RETURNING cleanup_attempts`, c.Claim.ID).Scan(&c.Attempt) != nil {
			return ErrExecution
		}
		return nil
	})
	if err != nil {
		return VerificationCleanup{}, ErrExecution
	}
	return c, nil
}
func (db *DB) FinishVerificationCleanup(ctx context.Context, c VerificationCleanup, removed bool) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return ErrExecution
	}
	r, err := db.Pool.ExecContext(ctx, `UPDATE verification_requests SET cleanup_state=CASE WHEN EXISTS(SELECT 1 FROM verification_effects e WHERE e.request_id=$1 AND e.resource_uid IS NULL AND e.not_sent_at IS NULL) THEN 'uncertain' WHEN $5 THEN 'complete' WHEN cleanup_attempts>=3 THEN 'uncertain' ELSE 'pending' END WHERE id=$1 AND claim_token=$2 AND claim_until=$3 AND cleanup_attempts=$4 AND state='failed' AND cleanup_state='pending'`, c.Claim.ID, c.Claim.Token, c.Claim.Until, c.Attempt, removed)
	if err != nil {
		return ErrExecution
	}
	n, err := r.RowsAffected()
	if err != nil || n != 1 {
		return ErrExecution
	}
	return nil
}
func (db *DB) VerificationCleanupBlocked(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return true, ErrExecution
	}
	var blocked bool
	if db.Pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM verification_requests WHERE (state='failed' AND cleanup_state<>'complete') OR state='claimed')`).Scan(&blocked) != nil {
		return true, ErrExecution
	}
	return blocked, nil
}
func (db *DB) RejectQueuedVerification(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return ErrExecution
	}
	_, err := db.Pool.ExecContext(ctx, `UPDATE verification_requests SET state='failed',reason='rejected',cleanup_state='complete' WHERE id=$1 AND state='queued'`, id)
	if err != nil {
		return ErrExecution
	}
	return nil
}
