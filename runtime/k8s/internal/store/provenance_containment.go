package store

import (
	"context"
	"time"
)

// ProvenanceContainmentBlocked fences uncertain or expired inner-loop custody.
// A still-live inner claim is not itself uncertainty and runs in another process;
// it cannot monopolize the short provenance window. Inner reconcilers retain their
// stronger, unchanged exclusion checks for creating Kubernetes workloads.
func (db *DB) ProvenanceContainmentBlocked(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return true, ErrExecution
	}
	var blocked bool
	e := db.Pool.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM verification_requests WHERE cleanup_state='uncertain' OR (state='failed' AND cleanup_state<>'complete') OR (state='claimed' AND (claim_until IS NULL OR claim_until<=clock_timestamp() OR expires_at<=clock_timestamp())))
 OR EXISTS(SELECT 1 FROM review_runs WHERE cleanup_state='uncertain' OR (state='failed' AND cleanup_state<>'complete') OR (state='claimed' AND (claim_until IS NULL OR claim_until<=clock_timestamp() OR expires_at<=clock_timestamp())))`).Scan(&blocked)
	if e != nil {
		return true, ErrExecution
	}
	return blocked, nil
}
