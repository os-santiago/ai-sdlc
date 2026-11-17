package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The caller already holds the original budget lock. Repair allowance is a
// separate one-use ledger; the original counters and sealed state never reset.
func lockRepairModel(ctx context.Context, tx *sql.Tx, l *lockedModel) error {
	var child lockedModel
	var work, parentState, parentReason string
	var responseLimit int
	var expiry time.Time
	err := tx.QueryRowContext(ctx, `SELECT b.work_item_uuid,b.state,b.max_calls,b.max_input_bytes,b.max_output_bytes,
 b.reserved_calls,b.reserved_input_bytes,b.reserved_output_bytes,b.expires_at,b.response_limit
 FROM repair_model_budgets b WHERE b.allocation_id=$1 FOR UPDATE`, l.allocationID).Scan(&work, &child.state,
		&child.budget.Calls, &child.budget.InputBytes, &child.budget.OutputBytes,
		&child.budgetUsed.Calls, &child.budgetUsed.InputBytes, &child.budgetUsed.OutputBytes, &expiry, &responseLimit)
	if errors.Is(err, sql.ErrNoRows) {
		var bound bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM execution_repairs WHERE child_allocation_id=$1)`, l.allocationID).Scan(&bound); err != nil || bound {
			return errModelPolicy
		}
		return nil
	}
	if err != nil {
		return err
	}
	var linked bool
	err = tx.QueryRowContext(ctx, `SELECT b.state,b.stop_reason,
 r.work_item_uuid=$2::uuid AND c.allocation_id=r.parent_allocation_id AND e.work_item_uuid=r.work_item_uuid
 AND a.work_item_uuid=r.work_item_uuid AND a.attempt=2 AND a.response_limit=$3
 AND m.request_id=r.child_request_id AND r.expires_at=$4 AND r.expires_at<=pa.expires_at
 AND r.expires_at<=e.expires_at AND pa.response_limit=$3
 FROM execution_repairs r JOIN model_budgets b ON b.work_item_uuid=r.work_item_uuid
 JOIN execution_candidates c ON c.request_id=r.parent_candidate_id
 JOIN execution_requests e ON e.id=c.request_id
 JOIN model_allocations pa ON pa.id=r.parent_allocation_id
 JOIN model_allocations a ON a.id=r.child_allocation_id
 JOIN execution_models m ON m.allocation_id=a.id
 WHERE r.child_allocation_id=$1`, l.allocationID, l.workItem, responseLimit, expiry).Scan(&parentState, &parentReason, &linked)
	if err != nil || !linked || work != l.workItem || parentState != "stopped" || parentReason != "candidate-captured" || expiry.After(l.budgetExpiry) ||
		child.budget.Calls > l.budget.Calls-l.budgetUsed.Calls || child.budget.InputBytes > l.budget.InputBytes-l.budgetUsed.InputBytes || child.budget.OutputBytes > l.budget.OutputBytes-l.budgetUsed.OutputBytes {
		return errModelPolicy
	}
	l.derived = true
	l.state, l.budget, l.budgetUsed, l.budgetExpiry = child.state, child.budget, child.budgetUsed, expiry
	return nil
}

// Both names are compile-time constants; no external identifier enters SQL.
func (l lockedModel) budgetTable() string {
	if l.derived {
		return "repair_model_budgets"
	}
	return "model_budgets"
}
