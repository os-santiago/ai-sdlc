package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
)

func (db *DB) LifecyclePodForModel(ctx context.Context, l Lifecycle) (pod string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = db.WithTx(ctx, func(tx *sql.Tx) error {
		phase, _, uid, e := lockLifecycle(ctx, tx, l)
		if e != nil || phase != "source-ready" || !modelUUID.MatchString(uid) {
			return ErrExecution
		}
		pod = uid
		return lifecycleCurrent(ctx, tx, l.Claim)
	})
	if err != nil {
		return "", ErrExecution
	}
	return pod, nil
}

// ActivateLifecycleModel is persistence, not authority. The execution boundary
// derives these values from current reviewed inputs and the signed assignment.
// A single transaction owns ready -> running and the immutable model allocation.
func (db *DB) ActivateLifecycleModel(ctx context.Context, l Lifecycle, b ModelBudget, a ModelAllocation, configDigest string) (ModelScope, error) {
	if !modelUUID.MatchString(a.ID) || !modelUUID.MatchString(b.WorkItem) || !modelSHA.MatchString(b.SourceSHA) || !modelDigest.MatchString(b.PolicyDigest) || !modelDigest.MatchString(configDigest) ||
		b.WorkItem != a.WorkItem || !b.ExpiresAt.Equal(a.ExpiresAt) || b.ExpiresAt.After(l.Claim.Until) || b.Limits != a.Limits ||
		!validModelLimits(a.Limits, 32, 4194304, 8388608) || a.ResponseLimit < 1 || a.ResponseLimit > modelmailbox.MaxResponseBytes || int64(a.ResponseLimit) > a.Limits.OutputBytes || len(b.Model) < 1 || len(b.Model) > 128 {
		return ModelScope{}, ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		phase, _, pod, err := lockLifecycle(ctx, tx, l)
		if err != nil || phase != "source-ready" || pod != a.PodUID {
			return ErrExecution
		}
		var work, repository string
		var attempt int
		if tx.QueryRowContext(ctx, `SELECT work_item_uuid,repository,attempt FROM execution_requests WHERE id=$1`, l.Claim.ID).Scan(&work, &repository, &attempt) != nil || work != b.WorkItem || repository != b.Repository || attempt != a.Attempt {
			return ErrExecution
		}
		// This row lock may wait. The final clock check below covers every wait,
		// including unique-index/FK waits while inserting the model records.
		result, err := tx.ExecContext(ctx, `UPDATE work_items SET status='running',updated_at=clock_timestamp() WHERE uuid=$1 AND status='ready'`, work)
		if err != nil {
			return ErrExecution
		}
		n, err := result.RowsAffected()
		if err != nil || n != 1 {
			return ErrExecution
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO model_budgets(work_item_uuid,repository,source_sha,policy_digest,model,max_calls,max_input_bytes,max_output_bytes,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, b.WorkItem, b.Repository, b.SourceSHA, b.PolicyDigest, b.Model, b.Limits.Calls, b.Limits.InputBytes, b.Limits.OutputBytes, b.ExpiresAt); err != nil {
			return ErrExecution
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO model_allocations(id,work_item_uuid,attempt,pod_uid,max_calls,max_input_bytes,max_output_bytes,response_limit,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, a.ID, a.WorkItem, a.Attempt, a.PodUID, a.Limits.Calls, a.Limits.InputBytes, a.Limits.OutputBytes, a.ResponseLimit, a.ExpiresAt); err != nil {
			return ErrExecution
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO execution_models(request_id,allocation_id,config_digest) VALUES($1,$2,$3)`, l.Claim.ID, a.ID, configDigest); err != nil {
			return ErrExecution
		}
		var live bool
		if tx.QueryRowContext(ctx, `SELECT $1::timestamptz>clock_timestamp()`, a.ExpiresAt).Scan(&live) != nil || !live {
			return ErrExecution
		}
		return lifecycleCurrent(ctx, tx, l.Claim)
	})
	if err != nil {
		return ModelScope{}, ErrExecution
	}
	return ModelScope{AllocationID: a.ID, WorkItem: b.WorkItem, PodUID: a.PodUID, Repository: b.Repository, SourceSHA: b.SourceSHA, PolicyDigest: b.PolicyDigest, Model: b.Model, Attempt: a.Attempt, ResponseLimit: a.ResponseLimit, ExpiresAt: a.ExpiresAt}, nil
}

// ReadLifecycleModel requires the exact live owner and immutable activation.
// It never adopts the owner or grants access to the model provider.
func (db *DB) ReadLifecycleModel(ctx context.Context, l Lifecycle, configDigest string) (scope ModelScope, phase string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = db.WithTx(ctx, func(tx *sql.Tx) error {
		var pod string
		phase, _, pod, err = lockLifecycle(ctx, tx, l)
		if err != nil {
			return ErrExecution
		}
		err = tx.QueryRowContext(ctx, `SELECT a.id,a.work_item_uuid,a.pod_uid,b.repository,b.source_sha,b.policy_digest,b.model,a.attempt,a.response_limit,LEAST(a.expires_at,b.expires_at)
 FROM execution_models e JOIN model_allocations a ON a.id=e.allocation_id JOIN model_budgets b ON b.work_item_uuid=a.work_item_uuid JOIN work_items w ON w.uuid=a.work_item_uuid JOIN execution_requests r ON r.id=e.request_id
 WHERE e.request_id=$1 AND e.config_digest=$2 AND w.status='running'
 AND ((b.state='active' AND $4<>'candidate-captured') OR (b.state='stopped' AND b.stop_reason='candidate-captured' AND $4='candidate-captured'
 AND EXISTS(SELECT 1 FROM execution_candidates c WHERE c.request_id=e.request_id AND c.allocation_id=a.id AND c.pod_uid=a.pod_uid AND c.source_sha=b.source_sha)))
 AND a.pod_uid=$3 AND r.work_item_uuid=a.work_item_uuid AND r.attempt=a.attempt AND r.repository=b.repository AND a.expires_at>clock_timestamp() AND b.expires_at>clock_timestamp()`, l.Claim.ID, configDigest, pod, phase).Scan(&scope.AllocationID, &scope.WorkItem, &scope.PodUID, &scope.Repository, &scope.SourceSHA, &scope.PolicyDigest, &scope.Model, &scope.Attempt, &scope.ResponseLimit, &scope.ExpiresAt)
		if err != nil || scope.ExpiresAt.After(l.Claim.Until) {
			return ErrExecution
		}
		return lifecycleCurrent(ctx, tx, l.Claim)
	})
	if err != nil {
		return ModelScope{}, "", ErrExecution
	}
	return scope, phase, nil
}
