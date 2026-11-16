package store

import (
	"context"
	"database/sql"
	"time"
)

// BuildEffect is a durable create intention. It is not a retry token. A lost
// create reply must quarantine the request/slot; never recreate or adopt a name.
type BuildEffect struct {
	Claim                      BuildClaim
	Token, RequestDigest, Name string
}

func (j *BuildJournal) BeginBuildEffect(ctx context.Context, c BuildClaim, digest string) (BuildEffect, error) {
	if !modelDigest.MatchString(digest) || !buildIDPattern.MatchString(c.ID) {
		return BuildEffect{}, ErrExecution
	}
	effect := BuildEffect{c, NewID(), digest, "ai-sdlc-build-" + c.ID[len("build-"):len("build-")+32]}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if j.lock(ctx, tx, c) != nil {
			return ErrExecution
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO build_effects(request_id,kind,token,request_digest,resource_name) VALUES($1,'build',$2,$3,$4)`, c.ID, effect.Token, digest, effect.Name); e != nil {
			return ErrExecution
		}
		return j.check(ctx, tx, c)
	})
	if e != nil {
		return BuildEffect{}, ErrExecution
	}
	return effect, nil
}
func (j *BuildJournal) AcknowledgeBuildCreated(ctx context.Context, effect BuildEffect, uid string) error {
	if !lifecycleUUID.MatchString(uid) || !lifecycleUUID.MatchString(effect.Token) || !modelDigest.MatchString(effect.RequestDigest) {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if j.lock(ctx, tx, effect.Claim) != nil {
			return ErrExecution
		}
		r, e := tx.ExecContext(ctx, `UPDATE build_effects SET resource_uid=$5,acknowledged_at=clock_timestamp() WHERE request_id=$1 AND kind='build' AND token=$2 AND request_digest=$3 AND resource_name=$4 AND resource_uid IS NULL`, effect.Claim.ID, effect.Token, effect.RequestDigest, effect.Name, uid)
		if e != nil {
			return ErrExecution
		}
		n, e := r.RowsAffected()
		if e != nil || n != 1 {
			return ErrExecution
		}
		return j.check(ctx, tx, effect.Claim)
	})
}
func (j *BuildJournal) ReadBuildResource(ctx context.Context, c BuildClaim) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var name, uid string
	e := j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if j.lock(ctx, tx, c) != nil {
			return ErrExecution
		}
		if tx.QueryRowContext(ctx, `SELECT resource_name,resource_uid FROM build_effects WHERE request_id=$1 AND kind='build' AND resource_uid IS NOT NULL`, c.ID).Scan(&name, &uid) != nil {
			return ErrExecution
		}
		return nil
	})
	if e != nil {
		return "", "", ErrExecution
	}
	return name, uid, nil
}
