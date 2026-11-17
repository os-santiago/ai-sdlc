package store

import (
	"context"
	"database/sql"
	"regexp"
	"time"
)

var failureContainerID = regexp.MustCompile(`^[a-z0-9-]{1,32}://[a-f0-9]{32,128}$`)

// RecordExecutionFailure retains a bounded observation before containment cleanup.
// It cannot capture a candidate, renew authority or reopen an attempt. Repeating
// the identical observation preserves its original database timestamp.
func (db *DB) RecordExecutionFailure(ctx context.Context, l Lifecycle, podUID, containerID string, code int32) error {
	if !lifecycleUUID.MatchString(podUID) || !failureContainerID.MatchString(containerID) || code < 1 || code > 255 {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		phase, _, originalPod, err := lockLifecycle(ctx, tx, l)
		if err != nil || phase != "start-ready" || originalPod != podUID {
			return ErrExecution
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO execution_failures(request_id,pod_uid,container_id,exit_code) VALUES($1,$2,$3,$4) ON CONFLICT(request_id) DO NOTHING`, l.Claim.ID, podUID, containerID, code); err != nil {
			return ErrExecution
		}
		var matches bool
		if tx.QueryRowContext(ctx, `SELECT pod_uid=$2 AND container_id=$3 AND exit_code=$4 FROM execution_failures WHERE request_id=$1`, l.Claim.ID, podUID, containerID, code).Scan(&matches) != nil || !matches {
			return ErrExecution
		}
		return lifecycleCurrent(ctx, tx, l.Claim)
	})
}
