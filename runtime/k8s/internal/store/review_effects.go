package store

import (
	"context"
	"database/sql"
	"time"
)

type ReviewEffect struct {
	Claim                          ReviewClaim
	Kind, Token, Name, UID, Digest string
	NotSent                        bool
}

func reviewEffectName(ctx context.Context, tx *sql.Tx, c ReviewClaim, kind string) (string, error) {
	var work, assignment string
	if tx.QueryRowContext(ctx, `SELECT work_item_uuid,assignment_digest FROM review_runs WHERE id=$1`, c.ID).Scan(&work, &assignment) != nil || !modelUUID.MatchString(work) || !modelDigest.MatchString(assignment) {
		return "", ErrExecution
	}
	switch kind {
	case "config":
		return "assignment-" + assignment[:32], nil
	case "pod", "import", "start", "export":
		return "executor-" + work + "-1", nil
	}
	return "", ErrExecution
}
func (db *DB) BeginReviewEffect(ctx context.Context, c ReviewClaim, kind string) (ReviewEffect, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	effect := ReviewEffect{Claim: c, Kind: kind, Token: NewID()}
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if lockReview(ctx, tx, c) != nil || reviewPending(ctx, tx, c) != nil {
			return ErrExecution
		}
		name, e := reviewEffectName(ctx, tx, c, kind)
		if e != nil {
			return e
		}
		effect.Name = name
		previous := map[string]string{"pod": "config", "import": "pod", "start": "import", "export": "start"}[kind]
		if previous != "" {
			var complete bool
			if tx.QueryRowContext(ctx, `SELECT completed_at IS NOT NULL FROM review_effects WHERE run_id=$1 AND kind=$2`, c.ID, previous).Scan(&complete) != nil || !complete {
				return ErrExecution
			}
		}
		if kind == "export" {
			var pending bool
			if tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM review_model_calls WHERE run_id=$1) OR EXISTS(SELECT 1 FROM review_model_calls WHERE run_id=$1 AND state<>'handed-off')`, c.ID).Scan(&pending) != nil || pending {
				return ErrExecution
			}
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO review_effects(run_id,kind,token,resource_name) VALUES($1,$2,$3,$4)`, c.ID, kind, effect.Token, name)
		if e != nil {
			return ErrExecution
		}
		return checkReviewTime(ctx, tx, c)
	})
	if err != nil {
		return ReviewEffect{}, ErrExecution
	}
	return effect, nil
}
func (db *DB) AcknowledgeReviewEffect(ctx context.Context, e ReviewEffect, uid, digest string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if !modelUUID.MatchString(uid) || digest != "" && !modelDigest.MatchString(digest) || (e.Kind == "import" || e.Kind == "export") && digest == "" {
		return ErrExecution
	}
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		if lockReview(ctx, tx, e.Claim) != nil || reviewPending(ctx, tx, e.Claim) != nil {
			return ErrExecution
		}
		name, err := reviewEffectName(ctx, tx, e.Claim, e.Kind)
		if err != nil || e.Name != name {
			return ErrExecution
		}
		if e.Kind != "config" && e.Kind != "pod" {
			var original string
			if tx.QueryRowContext(ctx, `SELECT resource_uid FROM review_effects WHERE run_id=$1 AND kind='pod'`, e.Claim.ID).Scan(&original) != nil || original != uid {
				return ErrExecution
			}
		}
		r, err := tx.ExecContext(ctx, `UPDATE review_effects SET resource_uid=$5,digest=NULLIF($6,''),completed_at=clock_timestamp() WHERE run_id=$1 AND kind=$2 AND token=$3 AND resource_name=$4 AND resource_uid IS NULL AND not_sent_at IS NULL`, e.Claim.ID, e.Kind, e.Token, e.Name, uid, digest)
		if err != nil {
			return ErrExecution
		}
		n, err := r.RowsAffected()
		if err != nil || n != 1 {
			return ErrExecution
		}
		return checkReviewTime(ctx, tx, e.Claim)
	})
}

// CancelUnsentReviewEffect is only for a caller that KNOWS it did not invoke the
// effect after recording intent. A timeout/lost reply is never a no-send proof.
func (db *DB) CancelUnsentReviewEffect(ctx context.Context, e ReviewEffect) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "verifier") != nil {
			return ErrExecution
		}
		var id string
		if tx.QueryRowContext(ctx, `SELECT id FROM review_runs WHERE id=$1 AND claim_token=$2 AND claim_until=$3 AND state IN ('claimed','failed') FOR UPDATE`, e.Claim.ID, e.Claim.Token, e.Claim.Until).Scan(&id) != nil {
			return ErrExecution
		}
		name, err := reviewEffectName(ctx, tx, e.Claim, e.Kind)
		if err != nil || name != e.Name {
			return ErrExecution
		}
		r, err := tx.ExecContext(ctx, `UPDATE review_effects SET not_sent_at=clock_timestamp() WHERE run_id=$1 AND kind=$2 AND token=$3 AND resource_name=$4 AND resource_uid IS NULL AND completed_at IS NULL AND not_sent_at IS NULL`, e.Claim.ID, e.Kind, e.Token, e.Name)
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
func (db *DB) ReadReviewEffects(ctx context.Context, c ReviewClaim) ([]ReviewEffect, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return nil, ErrExecution
	}
	rows, err := db.Pool.QueryContext(ctx, `SELECT e.kind,e.token,e.resource_name,COALESCE(e.resource_uid::text,''),COALESCE(e.digest,''),e.not_sent_at IS NOT NULL FROM review_effects e JOIN review_runs r ON r.id=e.run_id WHERE r.id=$1 AND r.claim_token=$2 AND r.claim_until=$3 ORDER BY e.kind LIMIT 6`, c.ID, c.Token, c.Until)
	if err != nil {
		return nil, ErrExecution
	}
	defer rows.Close()
	result := []ReviewEffect{}
	for rows.Next() {
		e := ReviewEffect{Claim: c}
		if rows.Scan(&e.Kind, &e.Token, &e.Name, &e.UID, &e.Digest, &e.NotSent) != nil {
			return nil, ErrExecution
		}
		result = append(result, e)
	}
	if rows.Err() != nil || len(result) > 5 {
		return nil, ErrExecution
	}
	return result, nil
}

// ConfirmReviewCleanup follows successful observation of removal of the exact
// acknowledged Pod/ConfigMap UIDs. Unknown create outcomes stay quarantined.
func (db *DB) ConfirmReviewCleanup(ctx context.Context, c ReviewClaim) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	complete := false
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "verifier") != nil {
			return ErrExecution
		}
		var id string
		if tx.QueryRowContext(ctx, `SELECT id FROM review_runs WHERE id=$1 AND claim_token=$2 AND claim_until=$3 AND state IN ('claimed','failed') AND cleanup_state='pending' FOR UPDATE`, c.ID, c.Token, c.Until).Scan(&id) != nil {
			return ErrExecution
		}
		if tx.QueryRowContext(ctx, `UPDATE review_runs SET cleanup_state=CASE WHEN EXISTS(SELECT 1 FROM review_effects WHERE run_id=$1 AND kind IN ('config','pod') AND resource_uid IS NULL AND not_sent_at IS NULL) THEN 'uncertain' ELSE 'complete' END WHERE id=$1 RETURNING cleanup_state='complete'`, c.ID).Scan(&complete) != nil {
			return ErrExecution
		}
		return nil
	})
	if err != nil || !complete {
		return ErrExecution
	}
	return nil
}
