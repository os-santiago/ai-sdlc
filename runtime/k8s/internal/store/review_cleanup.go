package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type ReviewCleanup struct {
	Claim   ReviewClaim
	Attempt int
}

func (db *DB) ExpireReviews(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return ErrExecution
	}
	_, err := db.Pool.ExecContext(ctx, `WITH expired AS (SELECT id FROM review_runs WHERE (state='queued' AND expires_at<=clock_timestamp()) OR (state='claimed' AND claim_until<=clock_timestamp()) ORDER BY created_at LIMIT 32 FOR UPDATE SKIP LOCKED) UPDATE review_runs r SET cleanup_state=CASE WHEN r.state='queued' THEN 'complete' ELSE r.cleanup_state END,state='failed',reason='expired' FROM expired e WHERE r.id=e.id`)
	if err != nil {
		return ErrExecution
	}
	return nil
}
func (db *DB) NextReviewCleanup(ctx context.Context) (ReviewCleanup, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var c ReviewCleanup
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "verifier") != nil {
			return ErrExecution
		}
		err := tx.QueryRowContext(ctx, `SELECT id,claim_token,claim_until FROM review_runs WHERE state='failed' AND cleanup_state='pending' AND claim_token IS NOT NULL AND cleanup_attempts<3 AND cleanup_next_at<=clock_timestamp() ORDER BY created_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&c.Claim.ID, &c.Claim.Token, &c.Claim.Until)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return ErrExecution
		}
		if tx.QueryRowContext(ctx, `UPDATE review_runs SET cleanup_attempts=cleanup_attempts+1,cleanup_next_at=clock_timestamp()+interval '30 seconds' WHERE id=$1 RETURNING cleanup_attempts`, c.Claim.ID).Scan(&c.Attempt) != nil {
			return ErrExecution
		}
		return nil
	})
	if err != nil {
		return ReviewCleanup{}, ErrExecution
	}
	return c, nil
}
func (db *DB) FinishReviewCleanup(ctx context.Context, c ReviewCleanup, removed bool) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return ErrExecution
	}
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		var id string
		if tx.QueryRowContext(ctx, `SELECT id FROM review_runs WHERE id=$1 AND claim_token=$2 AND claim_until=$3 AND cleanup_attempts=$4 AND state='failed' AND cleanup_state='pending' FOR UPDATE`, c.Claim.ID, c.Claim.Token, c.Claim.Until, c.Attempt).Scan(&id) != nil {
			return ErrExecution
		}
		_, e := tx.ExecContext(ctx, `UPDATE review_runs SET cleanup_state=CASE WHEN EXISTS(SELECT 1 FROM review_effects e WHERE e.run_id=$1 AND e.kind IN ('config','pod') AND e.resource_uid IS NULL AND e.not_sent_at IS NULL) THEN 'uncertain' WHEN $2 THEN 'complete' WHEN cleanup_attempts>=3 THEN 'uncertain' ELSE 'pending' END WHERE id=$1`, c.Claim.ID, removed)
		if e != nil {
			return ErrExecution
		}
		return nil
	})
}
func (db *DB) ReviewCleanupBlocked(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return true, ErrExecution
	}
	var blocked bool
	if db.Pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_runs WHERE state='claimed' OR (state='failed' AND cleanup_state<>'complete'))`).Scan(&blocked) != nil {
		return true, ErrExecution
	}
	return blocked, nil
}
