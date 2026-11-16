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

type PublicationSelection struct {
	AuthorityRevision, IntakeConfiguration, ExecutionProfile, ReviewProfile, ReviewConfiguration string
}

func (db *DB) NextPublicationHandoff(ctx context.Context, selection PublicationSelection) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for _, value := range []string{selection.AuthorityRevision, selection.IntakeConfiguration, selection.ExecutionProfile, selection.ReviewProfile, selection.ReviewConfiguration} {
		if !modelDigest.MatchString(value) {
			return "", ErrExecution
		}
	}
	if e := AssertComponent(ctx, db.Pool, "verifier"); e != nil {
		return "", identityObservationError(e)
	}
	var id string
	// Selection discards obsolete generations/configurations without modifying
	// historical proof or extending authority. CurrentProof still authorizes.
	e := db.Pool.QueryRowContext(ctx, `SELECT r.verification_id FROM review_runs r
 JOIN execution_requests e ON e.id=r.candidate_id
 JOIN work_items w ON w.uuid=e.work_item_uuid
 JOIN intake_heads h ON h.repository=e.repository
  AND h.issue_number=(convert_from(e.payload,'UTF8')::jsonb->>'issueNumber')::integer
  AND h.generation=(convert_from(e.payload,'UTF8')::jsonb->>'generation')::bigint
 JOIN intake_revisions i ON i.repository=h.repository AND i.issue_number=h.issue_number AND i.generation=h.generation
 WHERE r.state='passed' AND r.cleanup_state='complete' AND r.expires_at>clock_timestamp()
  AND e.authority_revision=$1 AND i.config_digest=$2
  AND convert_from(e.payload,'UTF8')::jsonb->>'profileSHA256'=$3
  AND r.profile_digest=$4 AND r.configuration_digest=$5 AND w.status='running'
  AND NOT EXISTS(SELECT 1 FROM publication_handoffs p WHERE p.review_id=r.id)
 ORDER BY r.created_at,r.id LIMIT 1`, selection.AuthorityRevision, selection.IntakeConfiguration, selection.ExecutionProfile, selection.ReviewProfile, selection.ReviewConfiguration).Scan(&id)
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
