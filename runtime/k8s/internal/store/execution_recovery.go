package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ExpireExecutionClaims is terminal reconciliation, never adoption. The claim
// lock fences late lifecycle effects; model locks fence late provider results.
func (db *DB) ExpireExecutionClaims(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "worker") != nil {
		return ErrExecution
	}
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT r.id,r.work_item_uuid,r.attempt FROM execution_requests r WHERE (r.state='claimed' AND (r.claim_until<=clock_timestamp() OR r.expires_at<=clock_timestamp())) OR (r.state='stopped' AND (EXISTS(SELECT 1 FROM model_budgets b WHERE b.work_item_uuid=r.work_item_uuid AND b.state='active' AND r.attempt=1) OR EXISTS(SELECT 1 FROM repair_model_budgets rb JOIN execution_repairs repair ON repair.child_allocation_id=rb.allocation_id WHERE repair.child_request_id=r.id AND rb.state IN ('reserved','active')) OR (EXISTS(SELECT 1 FROM work_items w WHERE w.uuid=r.work_item_uuid AND w.status IN ('ready','running')) AND NOT EXISTS(SELECT 1 FROM execution_candidates c WHERE c.request_id=r.id)) OR EXISTS(SELECT 1 FROM execution_models e JOIN tool_calls t ON t.allocation_id=e.allocation_id WHERE e.request_id=r.id AND t.state='started'))) ORDER BY r.claim_until LIMIT 32 FOR UPDATE OF r SKIP LOCKED`)
		if err != nil {
			return ErrExecution
		}
		type expired struct {
			id, work string
			attempt  int
		}
		var items []expired
		for rows.Next() {
			var v expired
			if rows.Scan(&v.id, &v.work, &v.attempt) != nil {
				rows.Close()
				return ErrExecution
			}
			items = append(items, v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return ErrExecution
		}
		for _, v := range items {
			if _, err = tx.ExecContext(ctx, `UPDATE execution_requests SET state='stopped',stop_reason='expired',updated_at=clock_timestamp() WHERE id=$1 AND state='claimed'`, v.id); err != nil {
				return err
			}
			// Serialize with original and derived calls without touching sealed
			// parent counters or allowing late parent recovery to cancel a child.
			if _, err = tx.ExecContext(ctx, `SELECT work_item_uuid FROM model_budgets WHERE work_item_uuid=$1 FOR UPDATE`, v.work); err != nil {
				return err
			}
			if v.attempt == 1 {
				_, err = tx.ExecContext(ctx, `UPDATE model_budgets SET state='stopped',stop_reason='execution-expired' WHERE work_item_uuid=$1 AND state='active'`, v.work)
			} else {
				_, err = tx.ExecContext(ctx, `UPDATE repair_model_budgets rb SET state='stopped',stop_reason='execution-expired' FROM execution_repairs repair WHERE repair.child_request_id=$1 AND repair.child_allocation_id=rb.allocation_id AND rb.state IN ('reserved','active')`, v.id)
			}
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE model_calls c SET state=CASE WHEN c.state='reserved' THEN 'stopped' ELSE 'ambiguous' END FROM model_allocations a WHERE c.allocation_id=a.id AND a.work_item_uuid=$1 AND (c.state IN ('reserved','started') OR (c.state='completed' AND c.response_handed_off_at IS NULL)) AND (($3=1 AND NOT EXISTS(SELECT 1 FROM repair_model_budgets rb WHERE rb.allocation_id=a.id)) OR ($3=2 AND EXISTS(SELECT 1 FROM execution_models e WHERE e.request_id=$2 AND e.allocation_id=a.id)))`, v.work, v.id, v.attempt); err != nil {
				return err
			}
			// A tool call left started by this execution never finishes: its
			// outcome is unknown, so it is abandoned (no run identity claimed).
			if _, err = tx.ExecContext(ctx, `UPDATE tool_calls t SET state='abandoned',finished_at=clock_timestamp() FROM execution_models e WHERE e.request_id=$1 AND t.allocation_id=e.allocation_id AND t.state='started'`, v.id); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE work_items SET status='failed',updated_at=clock_timestamp() WHERE uuid=$1 AND status IN ('ready','running') AND NOT EXISTS(SELECT 1 FROM execution_candidates WHERE request_id=$2)`, v.work, v.id); err != nil {
				return err
			}
		}
		return nil
	})
}

// CleanupRecord intentionally has no claim token, payload or execution method.
type CleanupRecord struct {
	RequestID, Namespace, PodName, ConfigName, PodUID, ConfigUID string
	Uncertain                                                    bool
}

func (db *DB) NextExecutionCleanup(ctx context.Context) (result CleanupRecord, err error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "worker") != nil {
		return result, ErrExecution
	}
	err = db.WithTx(ctx, func(tx *sql.Tx) error {
		e := tx.QueryRowContext(ctx, `SELECT l.request_id,l.namespace,l.pod_name,l.config_name,COALESCE(l.pod_uid::text,''),COALESCE(l.config_uid::text,''),
 EXISTS(SELECT 1 FROM execution_effects e WHERE e.request_id=l.request_id AND e.kind IN ('config','pod') AND e.result IS NULL)
 FROM execution_lifecycles l JOIN execution_requests r ON r.id=l.request_id WHERE r.state='stopped' AND l.cleanup_state='pending' AND l.cleanup_attempts<3 AND l.cleanup_after<=clock_timestamp() ORDER BY l.cleanup_after LIMIT 1 FOR UPDATE OF l SKIP LOCKED`).Scan(&result.RequestID, &result.Namespace, &result.PodName, &result.ConfigName, &result.PodUID, &result.ConfigUID, &result.Uncertain)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return ErrExecution
		}
		_, e = tx.ExecContext(ctx, `UPDATE execution_lifecycles SET cleanup_attempts=cleanup_attempts+1,cleanup_after=clock_timestamp()+interval '30 seconds' WHERE request_id=$1`, result.RequestID)
		return e
	})
	return
}
func (db *DB) FinishExecutionCleanup(ctx context.Context, c CleanupRecord, removed bool) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "worker") != nil {
		return ErrExecution
	}
	state := "pending"
	if removed {
		state = "complete"
		if c.Uncertain {
			state = "uncertain"
		}
	}
	_, err := db.Pool.ExecContext(ctx, `UPDATE execution_lifecycles SET cleanup_state=$2 WHERE request_id=$1 AND namespace=$3 AND pod_name=$4 AND config_name=$5 AND COALESCE(pod_uid::text,'')=$6 AND COALESCE(config_uid::text,'')=$7 AND cleanup_state='pending'`, c.RequestID, state, c.Namespace, c.PodName, c.ConfigName, c.PodUID, c.ConfigUID)
	return err
}
func (db *DB) ExecutionCleanupBlocked(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "worker") != nil {
		return true, ErrExecution
	}
	var blocked bool
	err := db.Pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM execution_lifecycles l JOIN execution_requests r ON r.id=l.request_id WHERE r.state='stopped' AND l.cleanup_state<>'complete')`).Scan(&blocked)
	return blocked, err
}
