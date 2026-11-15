package store

import (
	"context"
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

// ReadCapturedCandidate is the worker's read-only view for signed handoff.
func (db *DB) ReadCapturedCandidate(ctx context.Context, id string) (VerificationCandidate, error) {
	return db.readCandidate(ctx, id, "worker")
}
func (db *DB) readCandidate(ctx context.Context, id, component string) (VerificationCandidate, error) {
	var result VerificationCandidate
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := AssertComponent(ctx, db.Pool, component); err != nil {
		return result, identityObservationError(err)
	}
	err := db.Pool.QueryRowContext(ctx, `SELECT c.request_id,c.allocation_id,c.pod_uid,c.source_sha,c.source_tree_sha256,c.candidate_tree_sha256,c.export_sha256,c.export_bytes,c.delta_sha256,c.delta,c.changes,c.model_calls,c.created_at,r.work_item_uuid,r.repository,r.payload,r.receipt
 FROM execution_candidates c JOIN execution_requests r ON r.id=c.request_id
 JOIN execution_lifecycles l ON l.request_id=c.request_id
 JOIN execution_models m ON m.request_id=c.request_id AND m.allocation_id=c.allocation_id
 JOIN model_allocations a ON a.id=c.allocation_id AND a.work_item_uuid=r.work_item_uuid AND a.pod_uid=c.pod_uid AND a.attempt=r.attempt
 JOIN model_budgets b ON b.work_item_uuid=r.work_item_uuid AND b.source_sha=c.source_sha AND b.repository=r.repository
 WHERE c.request_id=$1 AND l.phase='candidate-captured' AND l.cleanup_state='complete' AND l.pod_uid=c.pod_uid AND b.state='stopped' AND b.stop_reason='candidate-captured'`, id).Scan(&result.Record.RequestID, &result.Record.AllocationID, &result.Record.PodUID, &result.Record.SourceSHA, &result.Record.SourceTreeSHA256, &result.Record.CandidateTreeSHA256, &result.Record.ExportSHA256, &result.Record.ExportBytes, &result.Record.DeltaSHA256, &result.Delta, &result.Record.Changes, &result.Record.ModelCalls, &result.CreatedAt, &result.WorkItem, &result.Repository, &result.Payload, &result.Receipt)
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
	result, err := db.Pool.ExecContext(ctx, `UPDATE execution_lifecycles SET cleanup_state='complete' WHERE request_id=$1 AND claim_token=$2 AND namespace=$3 AND config_name=$4 AND pod_name=$5 AND config_uid=$6 AND pod_uid=$7 AND NOT EXISTS(SELECT 1 FROM execution_effects e WHERE e.request_id=$1 AND e.kind IN ('config','pod') AND e.result IS NULL)`, l.Claim.ID, l.Claim.Token, l.Binding.Namespace, l.Binding.ConfigName, l.Binding.PodName, configUID, podUID)
	if err != nil {
		return ErrExecution
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return ErrExecution
	}
	return nil
}
