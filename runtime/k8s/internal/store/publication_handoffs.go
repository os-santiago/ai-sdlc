package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

type PassedReview struct {
	Run             ReviewRun
	Proof, Behavior []byte
}

func (db *DB) NextPublicationHandoff(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if e := AssertComponent(ctx, db.Pool, "verifier"); e != nil {
		return "", identityObservationError(e)
	}
	var id string
	e := db.Pool.QueryRowContext(ctx, `SELECT r.verification_id FROM review_runs r WHERE r.state='passed' AND r.cleanup_state='complete' AND r.expires_at>clock_timestamp() AND NOT EXISTS(SELECT 1 FROM publication_handoffs h WHERE h.review_id=r.id) ORDER BY r.created_at,r.id LIMIT 1`).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	if e != nil {
		return "", observationError(e)
	}
	return id, nil
}
func (db *DB) ReadPassedReview(ctx context.Context, verification string) (PassedReview, error) {
	return db.readPassedReview(ctx, verification, "verifier")
}
func (db *DB) ReadPublicationReview(ctx context.Context, verification string) (PassedReview, error) {
	return db.readPassedReview(ctx, verification, "release-manager")
}
func (db *DB) readPassedReview(ctx context.Context, verification, component string) (PassedReview, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var v PassedReview
	if e := AssertComponent(ctx, db.Pool, component); e != nil {
		return v, identityObservationError(e)
	}
	e := db.Pool.QueryRowContext(ctx, `SELECT r.id,r.verification_id,r.candidate_id,r.work_item_uuid,r.behavior_digest,r.profile_digest,r.configuration_digest,r.assignment_digest,r.expires_at,r.proof,v.proof FROM review_runs r JOIN verification_requests v ON v.id=r.verification_id WHERE r.verification_id=$1 AND r.state='passed' AND r.cleanup_state='complete' AND v.state='passed' AND v.cleanup_state='complete' AND r.expires_at>clock_timestamp() AND v.expires_at>clock_timestamp()`, verification).Scan(&v.Run.ID, &v.Run.VerificationID, &v.Run.CandidateID, &v.Run.WorkItem, &v.Run.BehaviorDigest, &v.Run.ProfileDigest, &v.Run.ConfigurationDigest, &v.Run.AssignmentDigest, &v.Run.ExpiresAt, &v.Proof, &v.Behavior)
	if e != nil {
		return PassedReview{}, observationError(e)
	}
	if authority.Digest(v.Behavior) != v.Run.BehaviorDigest {
		return PassedReview{}, ErrExecution
	}
	return v, nil
}
func RecordPublicationHandoff(ctx context.Context, tx *sql.Tx, review, command, payloadDigest, proofDigest string) error {
	if AssertComponent(ctx, tx, "verifier") != nil || !publicationIDPattern.MatchString(command) || !modelDigest.MatchString(payloadDigest) || !modelDigest.MatchString(proofDigest) {
		return ErrExecution
	}
	var proof []byte
	if tx.QueryRowContext(ctx, `SELECT proof FROM review_runs WHERE id=$1 AND state='passed' AND cleanup_state='complete' AND expires_at>clock_timestamp() FOR SHARE`, review).Scan(&proof) != nil || authority.Digest(proof) != proofDigest {
		return ErrExecution
	}
	_, e := tx.ExecContext(ctx, `INSERT INTO publication_handoffs(review_id,command_id,payload_digest) VALUES($1,$2,$3)`, review, command, payloadDigest)
	if e != nil {
		return ErrExecution
	}
	return nil
}
