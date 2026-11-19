package store

import (
	"context"
	"database/sql"
	"time"
)

// RepairModelReservation reads a previously committed allowance. It cannot
// create a budget or renew the command, and binds the original model settings.
func (db *DB) RepairModelReservation(ctx context.Context, request, config string) (ModelBudget, ModelAllocation, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "worker") != nil {
		return ModelBudget{}, ModelAllocation{}, ErrExecution
	}
	return readRepairReservation(ctx, db.Pool, request, config)
}

func readRepairReservation(ctx context.Context, q Querier, request, config string) (b ModelBudget, a ModelAllocation, err error) {
	err = q.QueryRowContext(ctx, `SELECT p.work_item_uuid,p.repository,p.source_sha,p.policy_digest,p.model,
 c.max_calls,c.max_input_bytes,c.max_output_bytes,c.expires_at,c.allocation_id,c.response_limit
 FROM execution_repairs r JOIN repair_model_budgets c ON c.work_item_uuid=r.work_item_uuid AND c.allocation_id=r.child_allocation_id
 JOIN model_budgets p ON p.work_item_uuid=r.work_item_uuid
 JOIN execution_candidates parent ON parent.request_id=r.parent_candidate_id AND parent.allocation_id=r.parent_allocation_id
 JOIN execution_requests original ON original.id=parent.request_id AND original.work_item_uuid=p.work_item_uuid
 JOIN execution_models m ON m.request_id=parent.request_id AND m.allocation_id=parent.allocation_id
 JOIN model_allocations pa ON pa.id=parent.allocation_id AND pa.work_item_uuid=p.work_item_uuid
 WHERE r.child_request_id=$1 AND m.config_digest=$2 AND p.state='stopped' AND p.stop_reason='candidate-captured'
 AND c.state='reserved' AND c.reserved_calls=0 AND c.reserved_input_bytes=0 AND c.reserved_output_bytes=0
 AND c.expires_at=r.expires_at AND c.expires_at<=p.expires_at AND c.expires_at<=original.expires_at AND c.expires_at<=pa.expires_at
 AND c.response_limit=pa.response_limit AND c.max_calls<=p.max_calls-p.reserved_calls
 AND c.max_input_bytes<=p.max_input_bytes-p.reserved_input_bytes AND c.max_output_bytes<=p.max_output_bytes-p.reserved_output_bytes
 AND c.expires_at>clock_timestamp()`, request, config).Scan(&b.WorkItem, &b.Repository, &b.SourceSHA, &b.PolicyDigest, &b.Model,
		&b.Limits.Calls, &b.Limits.InputBytes, &b.Limits.OutputBytes, &b.ExpiresAt, &a.ID, &a.ResponseLimit)
	if err != nil {
		return ModelBudget{}, ModelAllocation{}, ErrExecution
	}
	a.WorkItem, a.Attempt, a.Limits, a.ExpiresAt = b.WorkItem, 2, b.Limits, b.ExpiresAt
	return b, a, nil
}

func (db *DB) ActivateRepairLifecycleModel(ctx context.Context, l Lifecycle, b ModelBudget, a ModelAllocation, config string) (ModelScope, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if a.Attempt != 2 || !a.ExpiresAt.Equal(b.ExpiresAt) || a.ExpiresAt.After(l.Claim.Until) || !modelUUID.MatchString(a.PodUID) {
		return ModelScope{}, ErrExecution
	}
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "worker") != nil {
			return ErrExecution
		}
		// Keep the existing execution lock order used by expiry reconciliation:
		// request/lifecycle first, then parent and derived budget. Derivation
		// never locks an execution request while holding the parent budget.
		phase, _, pod, err := lockLifecycle(ctx, tx, l)
		if err != nil || phase != "source-ready" || pod != a.PodUID {
			return ErrExecution
		}
		var work string
		if tx.QueryRowContext(ctx, `SELECT work_item_uuid FROM model_budgets WHERE work_item_uuid=$1 FOR UPDATE`, b.WorkItem).Scan(&work) != nil {
			return ErrExecution
		}
		if tx.QueryRowContext(ctx, `SELECT work_item_uuid FROM repair_model_budgets WHERE allocation_id=$1 FOR UPDATE`, a.ID).Scan(&work) != nil || work != b.WorkItem {
			return ErrExecution
		}
		expected, allocation, err := readRepairReservation(ctx, tx, l.Claim.ID, config)
		if err != nil || a.ExpiresAt.After(expected.ExpiresAt) {
			return ErrExecution
		}
		expected.ExpiresAt, allocation.ExpiresAt, allocation.PodUID = b.ExpiresAt, a.ExpiresAt, a.PodUID
		if b != expected || a != allocation {
			return ErrExecution
		}
		var valid bool
		if tx.QueryRowContext(ctx, `SELECT r.work_item_uuid=$2::uuid AND r.attempt=2 AND r.repository=$3 AND r.expires_at>=$4 FROM execution_requests r JOIN work_items w ON w.uuid=r.work_item_uuid WHERE r.id=$1 AND w.status='running' FOR SHARE OF w`, l.Claim.ID, b.WorkItem, b.Repository, a.ExpiresAt).Scan(&valid) != nil || !valid {
			return ErrExecution
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO model_allocations(id,work_item_uuid,attempt,pod_uid,max_calls,max_input_bytes,max_output_bytes,response_limit,expires_at) VALUES($1,$2,2,$3,$4,$5,$6,$7,$8)`, a.ID, a.WorkItem, a.PodUID, a.Limits.Calls, a.Limits.InputBytes, a.Limits.OutputBytes, a.ResponseLimit, a.ExpiresAt); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO execution_models(request_id,allocation_id,config_digest) VALUES($1,$2,$3)`, l.Claim.ID, a.ID, config); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE repair_model_budgets SET state='active' WHERE allocation_id=$1 AND state='reserved'`, a.ID); err != nil {
			return err
		}
		if tx.QueryRowContext(ctx, `SELECT $1::timestamptz>clock_timestamp()`, a.ExpiresAt).Scan(&valid) != nil || !valid {
			return ErrExecution
		}
		return lifecycleCurrent(ctx, tx, l.Claim)
	})
	if err != nil {
		return ModelScope{}, ErrExecution
	}
	return ModelScope{AllocationID: a.ID, WorkItem: b.WorkItem, PodUID: a.PodUID, Repository: b.Repository, SourceSHA: b.SourceSHA, PolicyDigest: b.PolicyDigest, Model: b.Model, Attempt: 2, ResponseLimit: a.ResponseLimit, ExpiresAt: a.ExpiresAt}, nil
}
