package store

import (
	"context"
	"database/sql"
	"time"
)

// VerificationCandidate is a read-only view of the generator's immutable
// artifact ledger. Only the independent verifier role may consume it.
type VerificationCandidate struct {
	Record                  CandidateRecord
	WorkItem, Repository    string
	Payload, Receipt, Delta []byte
	CreatedAt               time.Time
}

func (db *DB) ReadVerificationCandidate(ctx context.Context, id string) (VerificationCandidate, error) {
	return db.readCandidate(ctx, id, "verifier")
}

func ReadVerificationCandidateTx(ctx context.Context, tx *sql.Tx, id string) (VerificationCandidate, error) {
	if tx == nil || AssertComponent(ctx, tx, "verifier") != nil {
		return VerificationCandidate{}, ErrExecution
	}
	return readCandidate(ctx, tx, id)
}

// ReadPublicationCandidate exposes immutable generation evidence, never writes.
func (db *DB) ReadPublicationCandidate(ctx context.Context, id string) (VerificationCandidate, error) {
	return db.readCandidate(ctx, id, "release-manager")
}

// ReadCapturedCandidate is the worker's read-only view for signed handoff.
func (db *DB) ReadCapturedCandidate(ctx context.Context, id string) (VerificationCandidate, error) {
	return db.readCandidate(ctx, id, "worker")
}

// ReadRepairCandidate authenticates immutable captured history in the same
// transaction that reserves a single child allowance.
func ReadRepairCandidate(ctx context.Context, tx *sql.Tx, id string) (VerificationCandidate, error) {
	if tx == nil || AssertComponent(ctx, tx, "orchestrator") != nil {
		return VerificationCandidate{}, ErrExecution
	}
	return readCandidate(ctx, tx, id)
}
func (db *DB) readCandidate(ctx context.Context, id, component string) (VerificationCandidate, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := AssertComponent(ctx, db.Pool, component); err != nil {
		return VerificationCandidate{}, identityObservationError(err)
	}
	return readCandidate(ctx, db.Pool, id)
}

func readCandidate(ctx context.Context, q Querier, id string) (VerificationCandidate, error) {
	var result VerificationCandidate
	err := q.QueryRowContext(ctx, `SELECT c.request_id,c.allocation_id,c.pod_uid,c.source_sha,c.source_tree_sha256,c.candidate_tree_sha256,c.export_sha256,c.export_bytes,c.delta_sha256,c.delta,c.changes,c.model_calls,c.created_at,r.work_item_uuid,r.repository,r.payload,r.receipt
 FROM execution_candidates c JOIN execution_requests r ON r.id=c.request_id
 JOIN execution_lifecycles l ON l.request_id=c.request_id
 JOIN execution_models m ON m.request_id=c.request_id AND m.allocation_id=c.allocation_id
 JOIN model_allocations a ON a.id=c.allocation_id AND a.work_item_uuid=r.work_item_uuid AND a.pod_uid=c.pod_uid AND a.attempt=r.attempt
 JOIN model_budgets b ON b.work_item_uuid=r.work_item_uuid AND b.source_sha=c.source_sha AND b.repository=r.repository
 LEFT JOIN execution_repairs repair ON repair.child_request_id=r.id AND repair.child_allocation_id=a.id AND repair.work_item_uuid=r.work_item_uuid
 LEFT JOIN repair_model_budgets rb ON rb.allocation_id=a.id AND rb.work_item_uuid=r.work_item_uuid
 WHERE c.request_id=$1 AND l.phase='candidate-captured' AND l.cleanup_state='complete' AND l.pod_uid=c.pod_uid AND b.state='stopped' AND b.stop_reason='candidate-captured'
 AND ((r.attempt=1 AND rb.allocation_id IS NULL AND repair.child_request_id IS NULL)
 OR (r.attempt=2 AND repair.child_request_id=r.id AND rb.state='stopped' AND rb.stop_reason='candidate-captured' AND rb.expires_at=repair.expires_at AND a.expires_at<=rb.expires_at))`, id).Scan(&result.Record.RequestID, &result.Record.AllocationID, &result.Record.PodUID, &result.Record.SourceSHA, &result.Record.SourceTreeSHA256, &result.Record.CandidateTreeSHA256, &result.Record.ExportSHA256, &result.Record.ExportBytes, &result.Record.DeltaSHA256, &result.Delta, &result.Record.Changes, &result.Record.ModelCalls, &result.CreatedAt, &result.WorkItem, &result.Repository, &result.Payload, &result.Receipt)
	if err != nil {
		return VerificationCandidate{}, observationError(err)
	}
	return result, nil
}

// ConfirmExecutionCleanup records only the exact acknowledged resources held
// by the original in-memory owner, including after its execution lease expires.
// This closes a resource observation, never renews or resumes execution.
func (db *DB) ConfirmExecutionCleanup(ctx context.Context, l Lifecycle, configUID, podUID string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "worker") != nil || !lifecycleUUID.MatchString(configUID) || !lifecycleUUID.MatchString(podUID) {
		return ErrExecution
	}
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		// Containment keeps the original ownership after expiry. Do not use the
		// live-claim lock helper: removing known resources must remain possible.
		var id string
		if tx.QueryRowContext(ctx, `SELECT id FROM execution_requests WHERE id=$1 AND claim_token=$2 AND claim_until=$3 FOR UPDATE`, l.Claim.ID, l.Claim.Token, l.Claim.Until).Scan(&id) != nil {
			return ErrExecution
		}
		result, err := tx.ExecContext(ctx, `UPDATE execution_lifecycles SET cleanup_state='complete' WHERE request_id=$1 AND claim_token=$2 AND namespace=$3 AND config_name=$4 AND pod_name=$5 AND config_uid=$6 AND pod_uid=$7 AND NOT EXISTS(SELECT 1 FROM execution_effects e WHERE e.request_id=$1 AND e.kind IN ('config','pod') AND e.result IS NULL)`, l.Claim.ID, l.Claim.Token, l.Binding.Namespace, l.Binding.ConfigName, l.Binding.PodName, configUID, podUID)
		if err != nil {
			return ErrExecution
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ErrExecution
		}
		return closeCapturedExecution(ctx, tx, l, podUID)
	})
}

// Completion releases the single-active-attempt slot only after exact cleanup
// and sealed candidate history. A late cleanup records containment, not success
// or renewed authority. The generic StopExecutionClaim cannot call this path.
func closeCapturedExecution(ctx context.Context, tx *sql.Tx, l Lifecycle, podUID string) error {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT candidate_cleanup_close`); err != nil {
		return ErrExecution
	}
	result, err := tx.ExecContext(ctx, `UPDATE execution_requests r SET state='stopped',stop_reason='candidate-captured',updated_at=clock_timestamp()
 WHERE r.id=$1 AND r.claim_token=$2 AND r.claim_until=$3 AND r.state='claimed'
 AND r.claim_until>clock_timestamp() AND r.expires_at>clock_timestamp()
 AND EXISTS(SELECT 1 FROM execution_lifecycles l JOIN execution_candidates c ON c.request_id=l.request_id
 JOIN execution_models m ON m.request_id=c.request_id AND m.allocation_id=c.allocation_id
 JOIN model_allocations a ON a.id=c.allocation_id AND a.work_item_uuid=r.work_item_uuid AND a.attempt=r.attempt AND a.pod_uid=c.pod_uid
 JOIN model_budgets b ON b.work_item_uuid=a.work_item_uuid AND b.source_sha=c.source_sha AND b.repository=r.repository
 LEFT JOIN execution_repairs repair ON repair.child_request_id=r.id AND repair.child_allocation_id=a.id AND repair.work_item_uuid=r.work_item_uuid
 LEFT JOIN repair_model_budgets rb ON rb.allocation_id=a.id AND rb.work_item_uuid=r.work_item_uuid
 WHERE l.request_id=r.id AND l.phase='candidate-captured' AND l.cleanup_state='complete' AND l.pod_uid=$4::uuid AND c.pod_uid=l.pod_uid
 AND b.state='stopped' AND b.stop_reason='candidate-captured'
 AND ((r.attempt=1 AND repair.child_request_id IS NULL AND rb.allocation_id IS NULL)
 OR (r.attempt=2 AND repair.child_request_id=r.id AND rb.state='stopped' AND rb.stop_reason='candidate-captured')))`, l.Claim.ID, l.Claim.Token, l.Claim.Until, podUID)
	if err != nil {
		return ErrExecution
	}
	n, err := result.RowsAffected()
	if err != nil {
		return ErrExecution
	}
	if n == 1 {
		var live bool
		if tx.QueryRowContext(ctx, `SELECT claim_until>clock_timestamp() AND expires_at>clock_timestamp() FROM execution_requests WHERE id=$1`, l.Claim.ID).Scan(&live) != nil {
			return ErrExecution
		}
		if !live {
			// An effect/trigger may have waited after UPDATE's predicate. Keep
			// the cleanup fact, but discard a closure that finished too late.
			if _, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT candidate_cleanup_close`); err != nil {
				return ErrExecution
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `RELEASE SAVEPOINT candidate_cleanup_close`); err != nil {
		return ErrExecution
	}
	return nil
}
