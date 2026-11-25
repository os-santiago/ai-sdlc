package publicationflow

import (
	"context"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publication"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
)

// CurrentPublishedProof verifies historical signatures at the original terminal
// publication time, then independently checks the current issue/configuration.
// It never renews generation/review/publication claims or authorizes a merge.
func CurrentPublishedProof(ctx context.Context, db *store.DB, load verificationflow.PolicyLoader, review ReviewLoader, id string) (publication.Command, store.PublishedRecord, error) {
	return publishedProof(ctx, db, load, review, id, true)
}

// SentPublishedProof performs every check of CurrentPublishedProof except the
// current issue snapshot. It exists only for the merge controller after its
// single merge request may have been sent: a pull request that says
// "Closes #N" makes GitHub close the issue as part of that merge, and the
// intake then records the closure as a new generation. That observation is a
// consequence of the effect being recorded, not a revocation of it, so it must
// not turn an accomplished merge into an uncertain one. It never authorizes a
// send; every pre-send fence uses CurrentPublishedProof.
func SentPublishedProof(ctx context.Context, db *store.DB, load verificationflow.PolicyLoader, review ReviewLoader, id string) (publication.Command, store.PublishedRecord, error) {
	return publishedProof(ctx, db, load, review, id, false)
}

func publishedProof(ctx context.Context, db *store.DB, load verificationflow.PolicyLoader, review ReviewLoader, id string, currentIssue bool) (publication.Command, store.PublishedRecord, error) {
	deny := func() (publication.Command, store.PublishedRecord, error) {
		return publication.Command{}, store.PublishedRecord{}, publication.ErrPublication
	}
	if db == nil || load == nil || review == nil {
		return deny()
	}
	fact, e := db.ReadPublishedRecord(ctx, id)
	if e != nil {
		return deny()
	}
	p, config, profile, e := load()
	if e != nil {
		return deny()
	}
	var signed authority.Receipt
	if authority.DecodeStrict(fact.Receipt, &signed) != nil {
		return deny()
	}
	c, e := publication.Verify(p, signed, fact.Payload, fact.PublishedAt)
	if e != nil || c.ID() != id || !time.UnixMilli(c.ExpiresAt).Equal(fact.AuthorityExpiresAt) {
		return deny()
	}
	original, e := db.ReadPublicationVerification(ctx, c.VerificationID)
	if e != nil || authority.Digest(original.Payload) != original.PayloadDigest {
		return deny()
	}
	var verified authority.Receipt
	if authority.DecodeStrict(original.Receipt, &verified) != nil {
		return deny()
	}
	v, e := verification.Verify(p, verified, original.Payload, fact.PublishedAt)
	if e != nil || v.ID() != original.ID || v.CandidateID != original.CandidateID || publication.MatchVerification(c, v) != nil || !time.UnixMilli(v.ExpiresAt).Equal(original.ExpiresAt) {
		return deny()
	}
	candidate, e := db.ReadPublicationCandidate(ctx, c.CandidateID)
	if e != nil || verification.MatchLedger(v, candidate) != nil || candidate.CreatedAt.After(fact.PublishedAt) {
		return deny()
	}
	var generatedReceipt authority.Receipt
	if authority.DecodeStrict(candidate.Receipt, &generatedReceipt) != nil {
		return deny()
	}
	generated, e := execution.Verify(p, generatedReceipt, candidate.Payload, fact.PublishedAt)
	if e != nil || generated.ID() != c.CandidateID || generated.WorkItemID != c.WorkItemID || generated.Repository != c.Repository || generated.SourceSHA != c.SourceSHA || generated.Generation != c.Generation || generated.IssueNumber != c.IssueNumber || generated.ProfileSHA256 != profile || c.ExpiresAt > generated.ExpiresAt {
		return deny()
	}
	expected, e := review()
	if e != nil {
		return deny()
	}
	passed, e := db.ReadPublishedReview(ctx, id)
	if e != nil {
		return deny()
	}
	matched, e := matchProofsAt(v, passed, expected, fact.PublishedAt)
	if e != nil || matched != c {
		return deny()
	}
	if currentIssue {
		current, e := execution.PublicationSnapshot(ctx, db, p, config, generated)
		_, selected := verification.SelectRecipe(c.Repository, current.Issue.Body)
		if e != nil || current.Revision != c.IssueRevision || !selected {
			return deny()
		}
	}
	latest, latestConfig, latestProfile, e := load()
	if e != nil || latest.Revision() != p.Revision() || latestConfig.Digest() != config.Digest() || latestProfile != profile || ctx.Err() != nil {
		return deny()
	}
	latestReview, e := review()
	if e != nil || latestReview != expected {
		return deny()
	}
	return c, fact, nil
}
