package store

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

// ReviewRun belongs only to the independent verifier. It does not reuse a
// generation model budget or confer publication/merge authority.
type ReviewRun struct {
	ID, VerificationID, CandidateID, WorkItem                            string
	BehaviorDigest, ProfileDigest, ConfigurationDigest, AssignmentDigest string
	ExpiresAt                                                            time.Time
}
type ReviewClaim struct {
	ID, Token string
	Until     time.Time
	lifetime  *reviewClaimLifetime
}

// The original owner also holds a non-serializable lifetime. A failed stop
// cannot resurrect that owner if PostgreSQL becomes available again. Restarted
// processes cannot reconstruct/adopt this capability from persisted fields.
type reviewClaimLifetime struct {
	id, token string
	until     time.Time
	revoked   atomic.Bool
}

func (c ReviewClaim) ownsLifetime() bool {
	return c.lifetime != nil && c.lifetime.id == c.ID && c.lifetime.token == c.Token && c.lifetime.until.Equal(c.Until)
}
func (c ReviewClaim) live() bool { return c.ownsLifetime() && !c.lifetime.revoked.Load() }
func (c ReviewClaim) revoke() {
	if c.ownsLifetime() {
		c.lifetime.revoked.Store(true)
	}
}

func ReviewID(verification, behavior, profile, config, assignment string) string {
	if !verificationIDPattern.MatchString(verification) || !modelDigest.MatchString(behavior) || !modelDigest.MatchString(profile) || !modelDigest.MatchString(config) || !modelDigest.MatchString(assignment) {
		return ""
	}
	return "review-" + authority.Digest([]byte(verification+"\x00"+behavior+"\x00"+profile+"\x00"+config+"\x00"+assignment))
}

// QueueReview pins an already committed, cleaned behavior proof. The caller
// must independently verify its signature/current issue and reconstruct the
// exact assignment; this storage transition alone grants no execution authority.
func (db *DB) QueueReview(ctx context.Context, verification, behavior, profile, config, assignment string) (ReviewRun, error) {
	run := ReviewRun{ID: ReviewID(verification, behavior, profile, config, assignment), VerificationID: verification, BehaviorDigest: behavior, ProfileDigest: profile, ConfigurationDigest: config, AssignmentDigest: assignment}
	if run.ID == "" {
		return ReviewRun{}, ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "verifier") != nil {
			return ErrExecution
		}
		var proof []byte
		if tx.QueryRowContext(ctx, `SELECT v.candidate_id,e.work_item_uuid,v.proof,v.expires_at FROM verification_requests v JOIN execution_requests e ON e.id=v.candidate_id WHERE v.id=$1 AND v.state='passed' AND v.cleanup_state='complete' AND v.expires_at>clock_timestamp() FOR SHARE OF v`, verification).Scan(&run.CandidateID, &run.WorkItem, &proof, &run.ExpiresAt) != nil || authority.Digest(proof) != behavior {
			return ErrExecution
		}
		_, e := tx.ExecContext(ctx, `INSERT INTO review_runs(id,verification_id,candidate_id,work_item_uuid,behavior_digest,profile_digest,configuration_digest,assignment_digest,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, run.ID, run.VerificationID, run.CandidateID, run.WorkItem, behavior, profile, config, assignment, run.ExpiresAt)
		if e != nil {
			return ErrExecution
		}
		return nil
	})
	if err != nil {
		return ReviewRun{}, ErrExecution
	}
	return run, nil
}
func (db *DB) ReadReview(ctx context.Context, id string) (ReviewRun, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := AssertComponent(ctx, db.Pool, "verifier"); err != nil {
		return ReviewRun{}, identityObservationError(err)
	}
	var r ReviewRun
	err := db.Pool.QueryRowContext(ctx, `SELECT id,verification_id,candidate_id,work_item_uuid,behavior_digest,profile_digest,configuration_digest,assignment_digest,expires_at FROM review_runs WHERE id=$1`, id).Scan(&r.ID, &r.VerificationID, &r.CandidateID, &r.WorkItem, &r.BehaviorDigest, &r.ProfileDigest, &r.ConfigurationDigest, &r.AssignmentDigest, &r.ExpiresAt)
	if err != nil {
		return ReviewRun{}, observationError(err)
	}
	return r, nil
}
func (db *DB) NextReview(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return "", ErrExecution
	}
	var id string
	err := db.Pool.QueryRowContext(ctx, `SELECT id FROM review_runs WHERE state='queued' AND expires_at>clock_timestamp() ORDER BY created_at,id LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", ErrExecution
	}
	return id, nil
}
func (db *DB) ClaimReview(ctx context.Context, id, configuration string) (ReviewClaim, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c := ReviewClaim{ID: id, Token: NewID()}
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "verifier") != nil {
			return ErrExecution
		}
		if _, e := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(73421024)`); e != nil {
			return ErrExecution
		}
		var busy bool
		if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_runs WHERE state='claimed' OR (state='failed' AND cleanup_state<>'complete'))`).Scan(&busy) != nil || busy {
			return ErrExecution
		}
		var state string
		if tx.QueryRowContext(ctx, `SELECT state FROM review_runs WHERE id=$1 AND configuration_digest=$2 FOR UPDATE`, id, configuration).Scan(&state) != nil || state != "queued" {
			return ErrExecution
		}
		if tx.QueryRowContext(ctx, `UPDATE review_runs SET state='claimed',claim_token=$3,claim_until=LEAST(expires_at,clock_timestamp()+interval '300 seconds') WHERE id=$1 AND configuration_digest=$2 AND state='queued' AND expires_at>clock_timestamp() RETURNING claim_until`, id, configuration, c.Token).Scan(&c.Until) != nil {
			return ErrExecution
		}
		c.lifetime = &reviewClaimLifetime{id: c.ID, token: c.Token, until: c.Until}
		return checkReviewTime(ctx, tx, c)
	})
	if err != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = db.StopReview(cleanup, c, "uncertain")
		return ReviewClaim{}, ErrExecution
	}
	return c, nil
}
func lockReview(ctx context.Context, tx *sql.Tx, c ReviewClaim) error {
	if !c.live() {
		return ErrExecution
	}
	if AssertComponent(ctx, tx, "verifier") != nil {
		return ErrExecution
	}
	var id string
	if tx.QueryRowContext(ctx, `SELECT id FROM review_runs WHERE id=$1 AND claim_token=$2 AND claim_until=$3 AND state='claimed' FOR UPDATE`, c.ID, c.Token, c.Until).Scan(&id) != nil {
		return ErrExecution
	}
	return checkReviewTime(ctx, tx, c)
}

// Repeat after all potentially blocking writes, before committing an effect or
// invocation capability. Transaction-start timestamps are not authorization.
func checkReviewTime(ctx context.Context, tx *sql.Tx, c ReviewClaim) error {
	if !c.live() {
		return ErrExecution
	}
	var valid bool
	if tx.QueryRowContext(ctx, `SELECT claim_until>clock_timestamp() AND expires_at>clock_timestamp() FROM review_runs WHERE id=$1`, c.ID).Scan(&valid) != nil || !valid {
		return ErrExecution
	}
	return nil
}
func reviewPending(ctx context.Context, tx *sql.Tx, c ReviewClaim) error {
	var pending bool
	if tx.QueryRowContext(ctx, `SELECT cleanup_state='pending' FROM review_runs WHERE id=$1`, c.ID).Scan(&pending) != nil || !pending {
		return ErrExecution
	}
	return nil
}
func (db *DB) CheckReviewClaim(ctx context.Context, c ReviewClaim) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return db.WithTx(ctx, func(tx *sql.Tx) error { return lockReview(ctx, tx, c) })
}
func (db *DB) StopReview(ctx context.Context, c ReviewClaim, reason string) error {
	c.revoke()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil || (reason != "expired" && reason != "revoked" && reason != "uncertain" && reason != "rejected" && reason != "request-changes") {
		return ErrExecution
	}
	// A lost claim commit may leave Until unknown; the fresh token still scopes
	// this stop to its own attempt and can never stop another claimant.
	r, e := db.Pool.ExecContext(ctx, `UPDATE review_runs SET state='failed',reason=$3 WHERE id=$1 AND claim_token=$2 AND state='claimed'`, c.ID, c.Token, reason)
	if e != nil {
		return ErrExecution
	}
	n, e := r.RowsAffected()
	if e != nil || n != 1 {
		return ErrExecution
	}
	return nil
}
