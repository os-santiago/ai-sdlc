package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrExecution = errors.New("execution request unavailable or conflicting")

type ExecutionRequest struct {
	ID, WorkItem, Repository, AuthorityRevision, PayloadDigest string
	Attempt                                                    int
	Payload, Receipt                                           []byte
	ExpiresAt                                                  time.Time
}
type ExecutionClaim struct {
	ID, Token string
	Until     time.Time
}

// StopExecutionClaim never stops another claimant or reopens an attempt.
func (db *DB) StopExecutionClaim(ctx context.Context, claim ExecutionClaim, reason string) error {
	if reason != "aborted" && reason != "expired" && reason != "authority-revoked" && reason != "uncertain" {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "worker") != nil {
		return ErrExecution
	}
	result, err := db.Pool.ExecContext(ctx, `UPDATE execution_requests SET state='stopped',stop_reason=$3,updated_at=clock_timestamp()
		WHERE id=$1 AND claim_token=$2 AND state='claimed'`, claim.ID, claim.Token, reason)
	if err != nil {
		return ErrExecution
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return ErrExecution
	}
	return nil
}

// InsertExecution is only called from the signed inbox transaction. Exact
// receipt/payload bytes remain immutable; the inbox handles legitimate replay.
func InsertExecution(ctx context.Context, tx *sql.Tx, r ExecutionRequest) error {
	if tx == nil || AssertComponent(ctx, tx, "worker") != nil {
		return ErrExecution
	}
	if r.Attempt == 2 {
		var reserved bool
		if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM execution_repairs repair JOIN repair_model_budgets b USING(work_item_uuid) JOIN work_items w ON w.uuid=repair.work_item_uuid
 WHERE repair.child_request_id=$1 AND repair.work_item_uuid=$2 AND repair.child_payload=$3 AND repair.expires_at=$4
 AND b.allocation_id=repair.child_allocation_id AND b.state='reserved' AND b.expires_at=repair.expires_at AND w.status='running')`, r.ID, r.WorkItem, r.Payload, r.ExpiresAt).Scan(&reserved) != nil || !reserved {
			return ErrExecution
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO execution_requests
        (id,work_item_uuid,attempt,repository,authority_revision,payload_digest,payload,receipt,expires_at)
        SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9 WHERE $9 > clock_timestamp()`,
		r.ID, r.WorkItem, r.Attempt, r.Repository, r.AuthorityRevision, r.PayloadDigest, r.Payload, r.Receipt, r.ExpiresAt)
	if err != nil {
		return ErrExecution
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return ErrExecution
	}
	if r.Attempt == 2 {
		var live bool
		if tx.QueryRowContext(ctx, `SELECT r.expires_at>clock_timestamp() AND w.status='running' FROM execution_requests r JOIN work_items w ON w.uuid=r.work_item_uuid WHERE r.id=$1`, r.ID).Scan(&live) != nil || !live {
			return ErrExecution
		}
	}
	return nil
}

func (db *DB) ReadExecution(ctx context.Context, id string) (ExecutionRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var r ExecutionRequest
	if AssertComponent(ctx, db.Pool, "worker") != nil {
		return r, ErrExecution
	}
	err := db.Pool.QueryRowContext(ctx, `SELECT id,work_item_uuid,attempt,repository,authority_revision,payload_digest,payload,receipt,expires_at
        FROM execution_requests WHERE id=$1`, id).Scan(&r.ID, &r.WorkItem, &r.Attempt, &r.Repository, &r.AuthorityRevision, &r.PayloadDigest, &r.Payload, &r.Receipt, &r.ExpiresAt)
	if err != nil {
		return ExecutionRequest{}, ErrExecution
	}
	return r, nil
}

// ClaimExecution grants only a freshly committed claim. Existing or expired
// claims are never reclaimed. A lost COMMIT acknowledgement cannot authorize
// an external effect; cleanup may stop only this caller's unique claim token.
func (db *DB) ClaimExecution(ctx context.Context, id, digest string) (ExecutionClaim, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	claim := ExecutionClaim{ID: id, Token: NewID()}
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "worker") != nil {
			return ErrExecution
		}
		var state string
		if tx.QueryRowContext(ctx, `SELECT state FROM execution_requests WHERE id=$1 AND payload_digest=$2 FOR UPDATE`, id, digest).Scan(&state) != nil || state != "queued" {
			return ErrExecution
		}
		// Evaluate actual database time after obtaining the row lock.
		err := tx.QueryRowContext(ctx, `UPDATE execution_requests SET state='claimed',claim_token=$3,
            claim_until=LEAST(expires_at,clock_timestamp()+interval '60 seconds'),updated_at=clock_timestamp()
            WHERE id=$1 AND payload_digest=$2 AND state='queued' AND expires_at>clock_timestamp()
            RETURNING claim_until`, id, digest, claim.Token).Scan(&claim.Until)
		if err != nil {
			return ErrExecution
		}
		return nil
	})
	if err != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		// No row with another claimant's token can be changed here.
		_, _ = db.Pool.ExecContext(cleanup, `UPDATE execution_requests SET state='stopped',stop_reason='uncertain',updated_at=clock_timestamp()
            WHERE id=$1 AND claim_token=$2 AND state='claimed'`, id, claim.Token)
		return ExecutionClaim{}, ErrExecution
	}
	return claim, nil
}

// ReadClaimedExecution never renews or adopts a claim. Every supplied binding
// must still match the durable row and actual database time at this read.
func (db *DB) ReadClaimedExecution(ctx context.Context, claim ExecutionClaim) (ExecutionRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var r ExecutionRequest
	if AssertComponent(ctx, db.Pool, "worker") != nil {
		return r, ErrExecution
	}
	err := db.Pool.QueryRowContext(ctx, `SELECT id,work_item_uuid,attempt,repository,authority_revision,payload_digest,payload,receipt,expires_at
 FROM execution_requests WHERE id=$1 AND claim_token=$2 AND claim_until=$3
 AND state='claimed' AND claim_until>clock_timestamp() AND expires_at>clock_timestamp()`, claim.ID, claim.Token, claim.Until).Scan(
		&r.ID, &r.WorkItem, &r.Attempt, &r.Repository, &r.AuthorityRevision, &r.PayloadDigest, &r.Payload, &r.Receipt, &r.ExpiresAt)
	if err != nil {
		return ExecutionRequest{}, ErrExecution
	}
	return r, nil
}
