package store

import (
	"context"
	"database/sql"
	"regexp"
	"time"
)

var verificationIDPattern = regexp.MustCompile(`^verification-[a-f0-9]{64}$`)

type VerificationEffect struct {
	Claim                  VerificationClaim
	Kind, Token, Name, UID string
	NotSent                bool
}

func lockVerification(ctx context.Context, tx *sql.Tx, c VerificationClaim) error {
	if AssertComponent(ctx, tx, "verifier") != nil {
		return ErrExecution
	}
	var id string
	if tx.QueryRowContext(ctx, `SELECT id FROM verification_requests WHERE id=$1 AND claim_token=$2 AND claim_until=$3 AND state='claimed' FOR UPDATE`, c.ID, c.Token, c.Until).Scan(&id) != nil {
		return ErrExecution
	}
	var valid bool
	if tx.QueryRowContext(ctx, `SELECT claim_until>clock_timestamp() AND expires_at>clock_timestamp() FROM verification_requests WHERE id=$1`, c.ID).Scan(&valid) != nil || !valid {
		return ErrExecution
	}
	return nil
}
func verificationResourceName(id, kind string) string {
	if !verificationIDPattern.MatchString(id) {
		return ""
	}
	suffix := kind
	switch kind {
	case "config":
		suffix = "inputs"
	case "baseline", "candidate", "reference", "tests":
	default:
		return ""
	}
	return "verify-" + id[13:45] + "-" + suffix
}
func (db *DB) BeginVerificationEffect(ctx context.Context, c VerificationClaim, kind string) (VerificationEffect, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result := VerificationEffect{Claim: c, Kind: kind, Token: NewID(), Name: verificationResourceName(c.ID, kind)}
	if result.Name == "" {
		return VerificationEffect{}, ErrExecution
	}
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if lockVerification(ctx, tx, c) != nil {
			return ErrExecution
		}
		if kind != "config" {
			var ready bool
			if tx.QueryRowContext(ctx, `SELECT resource_uid IS NOT NULL FROM verification_effects WHERE request_id=$1 AND kind='config'`, c.ID).Scan(&ready) != nil || !ready {
				return ErrExecution
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO verification_effects(request_id,kind,token,resource_name) VALUES($1,$2,$3,$4)`, c.ID, kind, result.Token, result.Name); err != nil {
			return ErrExecution
		}
		return nil
	})
	if err != nil {
		return VerificationEffect{}, ErrExecution
	}
	return result, nil
}
func (db *DB) AcknowledgeVerificationEffect(ctx context.Context, e VerificationEffect, uid string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if !lifecycleUUID.MatchString(uid) || e.Name != verificationResourceName(e.Claim.ID, e.Kind) {
		return ErrExecution
	}
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		if lockVerification(ctx, tx, e.Claim) != nil {
			return ErrExecution
		}
		r, err := tx.ExecContext(ctx, `UPDATE verification_effects SET resource_uid=$5,completed_at=clock_timestamp() WHERE request_id=$1 AND kind=$2 AND token=$3 AND resource_name=$4 AND resource_uid IS NULL AND not_sent_at IS NULL`, e.Claim.ID, e.Kind, e.Token, e.Name, uid)
		if err != nil {
			return ErrExecution
		}
		n, err := r.RowsAffected()
		if err != nil || n != 1 {
			return ErrExecution
		}
		return nil
	})
}
func (db *DB) ReadVerificationEffects(ctx context.Context, c VerificationClaim) ([]VerificationEffect, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return nil, ErrExecution
	}
	rows, err := db.Pool.QueryContext(ctx, `SELECT e.kind,e.token,e.resource_name,COALESCE(e.resource_uid::text,''),e.not_sent_at IS NOT NULL FROM verification_effects e JOIN verification_requests r ON r.id=e.request_id WHERE r.id=$1 AND r.claim_token=$2 AND r.claim_until=$3 ORDER BY e.kind LIMIT 6`, c.ID, c.Token, c.Until)
	if err != nil {
		return nil, ErrExecution
	}
	defer rows.Close()
	effects := []VerificationEffect{}
	for rows.Next() {
		e := VerificationEffect{Claim: c}
		if rows.Scan(&e.Kind, &e.Token, &e.Name, &e.UID, &e.NotSent) != nil || e.Name != verificationResourceName(c.ID, e.Kind) {
			return nil, ErrExecution
		}
		effects = append(effects, e)
	}
	if rows.Err() != nil || len(effects) > 5 {
		return nil, ErrExecution
	}
	return effects, nil
}

// This closes cleanup after the caller has confirmed removal of every issued
// resource UID. An unacknowledged effect cannot be turned into confirmed cleanup.
func (db *DB) ConfirmVerificationCleanup(ctx context.Context, c VerificationClaim) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return ErrExecution
	}
	r, err := db.Pool.ExecContext(ctx, `UPDATE verification_requests SET cleanup_state='complete' WHERE id=$1 AND claim_token=$2 AND claim_until=$3 AND state IN ('claimed','failed') AND NOT EXISTS(SELECT 1 FROM verification_effects e WHERE e.request_id=$1 AND e.resource_uid IS NULL AND e.not_sent_at IS NULL)`, c.ID, c.Token, c.Until)
	if err != nil {
		return ErrExecution
	}
	n, err := r.RowsAffected()
	if err != nil || n != 1 {
		return ErrExecution
	}
	return nil
}

// CancelUnsentVerificationEffect is called only before apply has been invoked.
// It closes an observation, not an execution lease: the exact original claim
// token may seal this fact after expiry. No delete or effect replay is allowed.
func (db *DB) CancelUnsentVerificationEffect(ctx context.Context, e VerificationEffect) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil || e.Name != verificationResourceName(e.Claim.ID, e.Kind) {
		return ErrExecution
	}
	r, err := db.Pool.ExecContext(ctx, `UPDATE verification_effects e SET not_sent_at=clock_timestamp()
      FROM verification_requests r WHERE e.request_id=r.id AND r.id=$1 AND r.claim_token=$2 AND r.claim_until=$3
      AND r.state IN ('claimed','failed') AND e.kind=$4 AND e.token=$5 AND e.resource_name=$6
      AND e.resource_uid IS NULL AND e.not_sent_at IS NULL`, e.Claim.ID, e.Claim.Token, e.Claim.Until, e.Kind, e.Token, e.Name)
	if err != nil {
		return ErrExecution
	}
	n, err := r.RowsAffected()
	if err != nil || n != 1 {
		return ErrExecution
	}
	return nil
}
