package store

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"time"
)

// ReadPublishedReview only exposes the original passed chain at an already
// committed publication time. It does not reactivate expired review claims.
func (db *DB) ReadPublishedReview(ctx context.Context, publication string) (PassedReview, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var v PassedReview
	if !publicationIDPattern.MatchString(publication) || AssertComponent(ctx, db.Pool, "release-manager") != nil {
		return v, ErrExecution
	}
	e := db.Pool.QueryRowContext(ctx, `SELECT r.id,r.verification_id,r.candidate_id,r.work_item_uuid,r.behavior_digest,r.profile_digest,r.configuration_digest,r.assignment_digest,r.expires_at,r.proof,v.proof FROM published_records f JOIN publication_requests p ON p.id=f.publication_id JOIN review_runs r ON r.id=p.review_id JOIN verification_requests v ON v.id=r.verification_id WHERE f.publication_id=$1 AND p.state='published' AND p.verification_id=v.id AND p.candidate_id=r.candidate_id AND r.state='passed' AND r.cleanup_state='complete' AND v.state='passed' AND v.cleanup_state='complete' AND r.expires_at>f.published_at AND v.expires_at>f.published_at AND f.published_at<=clock_timestamp()`, publication).Scan(&v.Run.ID, &v.Run.VerificationID, &v.Run.CandidateID, &v.Run.WorkItem, &v.Run.BehaviorDigest, &v.Run.ProfileDigest, &v.Run.ConfigurationDigest, &v.Run.AssignmentDigest, &v.Run.ExpiresAt, &v.Proof, &v.Behavior)
	if e != nil {
		return PassedReview{}, observationError(e)
	}
	if authority.Digest(v.Behavior) != v.Run.BehaviorDigest {
		return PassedReview{}, ErrExecution
	}
	return v, nil
}
