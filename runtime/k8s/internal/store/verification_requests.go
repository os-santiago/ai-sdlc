package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type VerificationRequest struct {
	ID, CandidateID, PayloadDigest string
	Payload, Receipt               []byte
	ExpiresAt                      time.Time
}
type VerificationClaim struct {
	ID, Token string
	Until     time.Time
}

func InsertVerification(ctx context.Context, tx *sql.Tx, r VerificationRequest) error {
	if tx == nil || AssertComponent(ctx, tx, "verifier") != nil {
		return ErrExecution
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO verification_requests(id,candidate_id,payload_digest,payload,receipt,expires_at) SELECT $1,$2,$3,$4,$5,$6 WHERE $6>clock_timestamp() AND EXISTS(SELECT 1 FROM verification_handoffs WHERE candidate_id=$2 AND command_id=$1 AND payload_digest=$3 AND state='sent')`, r.ID, r.CandidateID, r.PayloadDigest, r.Payload, r.Receipt, r.ExpiresAt)
	if err != nil {
		return ErrExecution
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return ErrExecution
	}
	return nil
}
func (db *DB) ReadVerification(ctx context.Context, id string) (VerificationRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var r VerificationRequest
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return r, ErrExecution
	}
	err := db.Pool.QueryRowContext(ctx, `SELECT id,candidate_id,payload_digest,payload,receipt,expires_at FROM verification_requests WHERE id=$1`, id).Scan(&r.ID, &r.CandidateID, &r.PayloadDigest, &r.Payload, &r.Receipt, &r.ExpiresAt)
	if err != nil {
		return VerificationRequest{}, ErrExecution
	}
	return r, nil
}
func (db *DB) NextVerification(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return "", ErrExecution
	}
	var id string
	err := db.Pool.QueryRowContext(ctx, `SELECT id FROM verification_requests WHERE state='queued' AND expires_at>clock_timestamp() ORDER BY created_at,id LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", ErrExecution
	}
	return id, nil
}

// A verification claim is exclusive and nonrenewable. Lost commit replies stop
// only this token and do not authorize creation or adoption of resources.
func (db *DB) ClaimVerification(ctx context.Context, id, digest string) (VerificationClaim, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	claim := VerificationClaim{ID: id, Token: NewID()}
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "verifier") != nil {
			return ErrExecution
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(73421023)`); err != nil {
			return ErrExecution
		}
		var busy bool
		if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM verification_requests WHERE state='claimed' OR (state='failed' AND cleanup_state<>'complete'))`).Scan(&busy) != nil || busy {
			return ErrExecution
		}
		var state string
		if tx.QueryRowContext(ctx, `SELECT state FROM verification_requests WHERE id=$1 AND payload_digest=$2 FOR UPDATE`, id, digest).Scan(&state) != nil || state != "queued" {
			return ErrExecution
		}
		if tx.QueryRowContext(ctx, `UPDATE verification_requests SET state='claimed',claim_token=$3,claim_until=LEAST(expires_at,clock_timestamp()+interval '180 seconds') WHERE id=$1 AND payload_digest=$2 AND state='queued' AND expires_at>clock_timestamp() RETURNING claim_until`, id, digest, claim.Token).Scan(&claim.Until) != nil {
			return ErrExecution
		}
		return nil
	})
	if err != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = db.StopVerification(cleanup, claim, "uncertain")
		return VerificationClaim{}, ErrExecution
	}
	return claim, nil
}
func (db *DB) CheckVerificationClaim(ctx context.Context, c VerificationClaim) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return ErrExecution
	}
	var valid bool
	if db.Pool.QueryRowContext(ctx, `SELECT state='claimed' AND claim_token=$2 AND claim_until=$3 AND claim_until>clock_timestamp() AND expires_at>clock_timestamp() FROM verification_requests WHERE id=$1`, c.ID, c.Token, c.Until).Scan(&valid) != nil || !valid {
		return ErrExecution
	}
	return nil
}
func (db *DB) StopVerification(ctx context.Context, c VerificationClaim, reason string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil || !verificationStopReason(reason) {
		return ErrExecution
	}
	result, err := db.Pool.ExecContext(ctx, `UPDATE verification_requests SET state='failed',reason=$3 WHERE id=$1 AND claim_token=$2 AND state='claimed'`, c.ID, c.Token, reason)
	if err != nil {
		return ErrExecution
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return ErrExecution
	}
	return nil
}
func verificationStopReason(reason string) bool {
	switch reason {
	case "expired", "revoked", "uncertain", "rejected", "comparison-failed":
		return true
	}
	return false
}
