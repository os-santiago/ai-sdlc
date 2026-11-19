package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

var ErrRepairExhausted = errors.New("observed repair capacity exhausted")
var ErrRepairExpired = errors.New("observed repair deadline expired")

// RepairAllowance is the sealed parent's unused capacity, never a new budget.
type RepairAllowance struct {
	Limits              ModelLimits
	OriginalLimits      ModelLimits
	ResponseLimit       int
	ConfigurationDigest string
	ExpiresAt           time.Time
}

// LockRepairParent retains the original lock until the signed inbox commits.
// Request/lifecycle rows are read only: taking their locks after the budget
// would invert the worker's cleanup/recovery lock order.
func LockRepairParent(ctx context.Context, tx *sql.Tx, work, candidate, allocation string) (RepairAllowance, error) {
	var a RepairAllowance
	if tx == nil || AssertComponent(ctx, tx, "orchestrator") != nil {
		return a, ErrExecution
	}
	if _, err := tx.ExecContext(ctx, `SELECT public.lock_repair_parent_budget($1::uuid)`, work); err != nil {
		return a, ErrExecution
	}
	a, err := readRepairAllowance(ctx, tx, work, candidate, allocation)
	if err != nil {
		return a, err
	}

	// A millisecond command must round DOWN; serialization cannot renew expiry.
	a.ExpiresAt = time.UnixMilli(a.ExpiresAt.UnixMilli())
	var status string
	if tx.QueryRowContext(ctx, `SELECT status FROM work_items WHERE uuid=$1 FOR UPDATE`, work).Scan(&status) != nil || status != "running" {
		return RepairAllowance{}, ErrExecution
	}
	if a.Limits.Calls < 1 || a.Limits.InputBytes < 1 || a.Limits.OutputBytes < int64(a.ResponseLimit) || !a.ExpiresAt.After(time.Now()) {
		return RepairAllowance{}, ErrExecution
	}
	return a, nil
}

func (db *DB) ReadRepairAllowance(ctx context.Context, work, candidate, allocation string) (RepairAllowance, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return RepairAllowance{}, ErrExecution
	}
	return readRepairAllowance(ctx, db.Pool, work, candidate, allocation)
}
func readRepairAllowance(ctx context.Context, q Querier, work, candidate, allocation string) (RepairAllowance, error) {
	var a RepairAllowance
	var live bool
	if q.QueryRowContext(ctx, `SELECT LEAST(b.max_calls-b.reserved_calls,a.max_calls-a.reserved_calls),
 LEAST(b.max_input_bytes-b.reserved_input_bytes,a.max_input_bytes-a.reserved_input_bytes),
 LEAST(b.max_output_bytes-b.reserved_output_bytes,a.max_output_bytes-a.reserved_output_bytes),
 b.max_calls,b.max_input_bytes,b.max_output_bytes,a.response_limit,m.config_digest,
 LEAST(b.expires_at,a.expires_at,e.expires_at,e.claim_until),LEAST(b.expires_at,a.expires_at,e.expires_at,e.claim_until)>clock_timestamp()
 FROM model_budgets b JOIN model_allocations a ON a.work_item_uuid=b.work_item_uuid
 JOIN execution_candidates c ON c.allocation_id=a.id AND c.request_id=$2
 JOIN execution_requests e ON e.id=c.request_id AND e.work_item_uuid=b.work_item_uuid
 JOIN execution_lifecycles l ON l.request_id=e.id
 JOIN execution_models m ON m.request_id=e.id AND m.allocation_id=a.id
 WHERE b.work_item_uuid=$1 AND a.id=$3 AND a.attempt=1 AND e.attempt=1
 AND e.state='stopped' AND e.stop_reason='candidate-captured'
 AND l.phase='candidate-captured' AND l.cleanup_state='complete' AND l.pod_uid=c.pod_uid AND c.pod_uid=a.pod_uid
 AND b.state='stopped' AND b.stop_reason='candidate-captured'
 AND b.repository=e.repository AND b.source_sha=c.source_sha
 AND NOT EXISTS(SELECT 1 FROM model_calls calls WHERE calls.allocation_id=a.id AND (calls.state<>'completed' OR calls.response_handed_off_at IS NULL))`, work, candidate, allocation).Scan(
		&a.Limits.Calls, &a.Limits.InputBytes, &a.Limits.OutputBytes, &a.OriginalLimits.Calls, &a.OriginalLimits.InputBytes, &a.OriginalLimits.OutputBytes, &a.ResponseLimit, &a.ConfigurationDigest, &a.ExpiresAt, &live) != nil {
		// Failure to observe is not proof of exhaustion. A transient SQL error
		// must never consume the unique terminal handoff for this review.
		return RepairAllowance{}, ErrObservationUnavailable
	}
	a.ExpiresAt = time.UnixMilli(a.ExpiresAt.UnixMilli())
	if !live || !a.ExpiresAt.After(time.Now()) {
		return RepairAllowance{}, ErrRepairExpired
	}
	if a.Limits.Calls < 1 || a.Limits.InputBytes < 1 || a.Limits.OutputBytes < int64(a.ResponseLimit) {
		return RepairAllowance{}, ErrRepairExhausted
	}
	return a, nil
}

// InsertRepairReservation and the execute-work outbox append share the caller's
// authenticated inbox transaction. Unique lineage keys prevent a second child.
func InsertRepairReservation(ctx context.Context, tx *sql.Tx, work, review, parent, parentAllocation, child, childAllocation, handoffDigest string, payload []byte, a RepairAllowance) error {
	if AssertComponent(ctx, tx, "orchestrator") != nil || len(payload) < 1 || len(payload) > 16384 || !modelDigest.MatchString(handoffDigest) {
		return ErrExecution
	}
	var found string
	if tx.QueryRowContext(ctx, `SELECT review_id FROM review_repair_handoffs WHERE review_id=$1 AND work_item_uuid=$2 AND outcome='forwarded' AND payload_digest=$3`, review, work, handoffDigest).Scan(&found) != nil {
		return ErrExecution
	}
	if _, e := tx.ExecContext(ctx, `INSERT INTO execution_repairs(work_item_uuid,parent_review_id,parent_candidate_id,parent_allocation_id,child_request_id,child_allocation_id,handoff_digest,child_payload,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, work, review, parent, parentAllocation, child, childAllocation, handoffDigest, payload, a.ExpiresAt); e != nil {
		return ErrExecution
	}
	if _, e := tx.ExecContext(ctx, `INSERT INTO repair_model_budgets(work_item_uuid,allocation_id,max_calls,max_input_bytes,max_output_bytes,response_limit,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, work, childAllocation, a.Limits.Calls, a.Limits.InputBytes, a.Limits.OutputBytes, a.ResponseLimit, a.ExpiresAt); e != nil {
		return ErrExecution
	}
	return nil
}

func CheckRepairReservation(ctx context.Context, tx *sql.Tx, work, child string, payload []byte) error {
	if AssertComponent(ctx, tx, "orchestrator") != nil {
		return ErrExecution
	}
	var raw []byte
	if tx.QueryRowContext(ctx, `SELECT r.child_payload FROM execution_repairs r JOIN repair_model_budgets b ON b.work_item_uuid=r.work_item_uuid AND b.allocation_id=r.child_allocation_id JOIN work_items w ON w.uuid=r.work_item_uuid
 WHERE r.work_item_uuid=$1 AND r.child_request_id=$2 AND r.expires_at>clock_timestamp() AND b.expires_at=r.expires_at AND b.state='reserved' AND w.status='running'
 AND NOT EXISTS(SELECT 1 FROM publication_handoffs p JOIN review_runs review ON review.id=p.review_id WHERE review.work_item_uuid=r.work_item_uuid)`, work, child).Scan(&raw) != nil || authority.Digest(raw) != authority.Digest(payload) {
		return ErrExecution
	}
	return nil
}
