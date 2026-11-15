package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// NextExecution returns only a queued identifier. Claim performs the durable
// CAS and signed/readiness revalidation; a queue read is never authority.
func (db *DB) NextExecution(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "worker") != nil {
		return "", ErrExecution
	}
	var id string
	err := db.Pool.QueryRowContext(ctx, `SELECT id FROM execution_requests WHERE state='queued' AND expires_at>clock_timestamp() ORDER BY created_at,id LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", ErrExecution
	}
	return id, nil
}
func (db *DB) RejectQueuedExecution(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "worker") != nil {
		return ErrExecution
	}
	_, err := db.Pool.ExecContext(ctx, `UPDATE execution_requests SET state='stopped',stop_reason='authority-revoked',updated_at=clock_timestamp() WHERE id=$1 AND state='queued'`, id)
	if err != nil {
		return ErrExecution
	}
	return nil
}
func (db *DB) ExpireQueuedExecutions(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "worker") != nil {
		return ErrExecution
	}
	_, err := db.Pool.ExecContext(ctx, `WITH expired AS (SELECT id FROM execution_requests WHERE state='queued' AND expires_at<=clock_timestamp() ORDER BY created_at LIMIT 32 FOR UPDATE SKIP LOCKED) UPDATE execution_requests r SET state='stopped',stop_reason='expired',updated_at=clock_timestamp() FROM expired e WHERE r.id=e.id`)
	if err != nil {
		return ErrExecution
	}
	return nil
}
