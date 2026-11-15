package store

import (
	"context"
	"database/sql"
	"time"
)

// AcknowledgeModelResponse records an exact response publication confirmed by a
// trusted, UID-bound mailbox helper. It is not proof that SCC received/consumed
// the response. Reconciliation never authorizes another provider invocation.
// Only a future trusted dispatcher may call it; no runtime role has grants.
func (db *DB) AcknowledgeModelResponse(ctx context.Context, ref ModelCallRef, responseSHA256 string, status, size int) (ModelDecision, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var d ModelDecision
	if !validModelRef(ref) || !modelDigest.MatchString(responseSHA256) || size < 0 || size > 262144 {
		return d, errModelPolicy
	}
	switch status {
	case 200, 400, 401, 403, 429, 500, 502, 503, 504:
	default:
		return d, errModelPolicy
	}
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		l, err := lockModel(ctx, tx, ref.AllocationID)
		if err != nil {
			return err
		}
		c, err := readModelCall(ctx, tx, ref)
		if err != nil {
			return err
		}
		if err = refreshModelTime(ctx, tx, &l); err != nil {
			return err
		}
		d.Call = c
		if l.podUID != ref.PodUID || c.RequestSHA256 != ref.RequestSHA256 {
			d.Reason = "binding-mismatch"
			return stopModel(ctx, tx, l, d.Reason)
		}
		if c.State != "completed" {
			d.Reason = "not-completed"
			return nil
		}
		var same bool
		if err = tx.QueryRowContext(ctx, `SELECT response_sha256=$3 AND response_status=$4 AND output_bytes=$5 FROM model_calls WHERE allocation_id=$1 AND sequence=$2`, ref.AllocationID, ref.Sequence, responseSHA256, status, size).Scan(&same); err != nil {
			return err
		}
		if !same {
			d.Reason = "handoff-mismatch"
			return stopModel(ctx, tx, l, d.Reason)
		}
		// An identical committed acknowledgement can be observed after expiration or
		// a later stop. It is metadata only: no timestamp or parent state is changed.
		if c.ResponseHandedOff {
			d.Reason = "existing"
			return nil
		}
		if !l.active() || !l.now.Before(c.ExpiresAt) {
			d.Reason = "uncertain-handoff"
			if err = stopModel(ctx, tx, l, d.Reason); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE model_calls SET state='ambiguous' WHERE allocation_id=$1 AND sequence=$2`, ref.AllocationID, ref.Sequence); err != nil {
				return err
			}
			d.Call.State = "ambiguous"
			return nil
		}
		if _, err = tx.ExecContext(ctx, `UPDATE model_calls SET response_handed_off_at=$3 WHERE allocation_id=$1 AND sequence=$2 AND state='completed' AND response_handed_off_at IS NULL`, ref.AllocationID, ref.Sequence, l.now); err != nil {
			return err
		}
		d.Call.ResponseHandedOff = true
		d.Reason = "handed-off"
		return nil
	})
	if err != nil {
		return ModelDecision{}, err
	}
	return d, nil
}
