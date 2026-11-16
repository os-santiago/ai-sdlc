package publicationflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publication"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
	"time"
)

type IdentityLoader func() (authority.Policy, authority.Identity, error)

// Handoff is called only by the verifier reconciler. An ambiguous COMMIT is not
// retried: the unique journal and outbox determine whether the handoff exists.
func Handoff(ctx context.Context, db *store.DB, policy verificationflow.PolicyLoader, review ReviewLoader, identity IdentityLoader) (bool, error) {
	if db == nil || policy == nil || review == nil || identity == nil {
		return false, publication.ErrPublication
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	expected, e := review()
	if e != nil {
		return false, publication.ErrPublication
	}
	selectedPolicy, intake, profile, e := policy()
	if e != nil {
		return false, store.ErrObservationUnavailable
	}
	id, e := db.NextPublicationHandoff(ctx, store.PublicationSelection{
		AuthorityRevision: selectedPolicy.Revision(), IntakeConfiguration: intake.Digest(),
		ExecutionProfile: profile, ReviewProfile: expected.Profile, ReviewConfiguration: expected.Configuration,
	})
	if e != nil || id == "" {
		return false, e
	}
	v, _, _, e := verificationflow.CurrentProof(ctx, db, policy, id)
	if e != nil {
		return false, e
	}
	passed, e := db.ReadPassedReview(ctx, id)
	if e != nil {
		return false, e
	}
	c, e := matchProofs(v, passed, expected)
	if e != nil {
		return false, e
	}
	raw, _ := json.Marshal(c)
	p, key, e := identity()
	if e != nil || key.Role != "verifier" || p.Revision() != c.AuthorityRevision {
		return false, publication.ErrPublication
	}
	signed, e := key.Seal(p, c.ID(), "publish-candidate", raw)
	if e != nil {
		return false, publication.ErrPublication
	}
	if _, e = publication.Verify(p, signed, raw, time.Now()); e != nil {
		return false, e
	}
	current, _, _, e := verificationflow.CurrentProof(ctx, db, policy, id)
	if e != nil || current != v {
		return false, publication.ErrPublication
	}
	latest, e := review()
	if e != nil || latest != expected {
		return false, publication.ErrPublication
	}
	latestPolicy, latestKey, e := identity()
	if e != nil || latestPolicy.Revision() != p.Revision() || latestKey.KeyID != key.KeyID || latestKey.Role != "verifier" {
		return false, publication.ErrPublication
	}
	e = db.WithTx(ctx, func(tx *sql.Tx) error {
		if e := store.RecordPublicationHandoff(ctx, tx, passed.Run.ID, c.ID(), authority.Digest(raw), c.IndependentReviewSHA256); e != nil {
			return e
		}
		return delivery.Append(ctx, tx, latestPolicy, latestKey, c.ID(), "publish-candidate", raw)
	})
	return e == nil, e
}

// Effect belongs exclusively to the signed component inbox. The transaction
// binds an existing verifier handoff and exact proof hashes, never raw issue text.
func Effect(load func() (authority.Policy, error)) delivery.Effect {
	return func(ctx context.Context, tx *sql.Tx, receipt authority.Receipt, raw []byte) ([]byte, error) {
		if load == nil {
			return nil, publication.ErrPublication
		}
		p, e := load()
		if e != nil {
			return nil, publication.ErrPublication
		}
		c, e := publication.Verify(p, receipt, raw, time.Now())
		if e != nil {
			return nil, e
		}
		var reviewID string
		if tx.QueryRowContext(ctx, `SELECT review_id FROM publication_handoffs WHERE command_id=$1 AND payload_digest=$2`, c.ID(), authority.Digest(raw)).Scan(&reviewID) != nil {
			return nil, publication.ErrPublication
		}
		sealed, _ := json.Marshal(receipt)
		r := store.PublicationRequest{ID: c.ID(), ReviewID: reviewID, VerificationID: c.VerificationID, CandidateID: c.CandidateID, Repository: c.Repository, PayloadDigest: authority.Digest(raw), Payload: raw, Receipt: sealed, ExpiresAt: time.UnixMilli(c.ExpiresAt)}
		if e = store.InsertPublication(ctx, tx, r, c.IndependentReviewSHA256, c.BehaviorProofSHA256); e != nil {
			return nil, e
		}
		return json.Marshal(map[string]string{"publicationId": c.ID(), "state": "queued"})
	}
}
