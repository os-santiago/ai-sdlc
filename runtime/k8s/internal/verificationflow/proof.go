package verificationflow

import (
	"context"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

type PolicyLoader func() (authority.Policy, intake.Config, string, error)

// CurrentProof is evidence for the next bounded step, never a reusable lease.
// Callers repeat it before each effect and before persisting acceptance.
func CurrentProof(ctx context.Context, db *store.DB, load PolicyLoader, id string) (verification.Command, store.VerificationCandidate, intake.Snapshot, error) {
	return currentProof(ctx, db, load, id, false)
}

// CurrentPublicationProof repeats the entire signed chain using read-only
// release-manager projections. No verifier or generator identity is borrowed.
func CurrentPublicationProof(ctx context.Context, db *store.DB, load PolicyLoader, id string) (verification.Command, store.VerificationCandidate, intake.Snapshot, error) {
	return currentProof(ctx, db, load, id, true)
}
func currentProof(ctx context.Context, db *store.DB, load PolicyLoader, id string, publication bool) (verification.Command, store.VerificationCandidate, intake.Snapshot, error) {
	reject := func() (verification.Command, store.VerificationCandidate, intake.Snapshot, error) {
		return verification.Command{}, store.VerificationCandidate{}, intake.Snapshot{}, verification.ErrVerification
	}
	unavailable := func() (verification.Command, store.VerificationCandidate, intake.Snapshot, error) {
		return verification.Command{}, store.VerificationCandidate{}, intake.Snapshot{}, store.ErrObservationUnavailable
	}
	readVerification := db.ReadVerification
	readCandidate := db.ReadVerificationCandidate
	readSnapshot := execution.CandidateSnapshot
	if publication {
		readVerification = db.ReadPublicationVerification
		readCandidate = db.ReadPublicationCandidate
		readSnapshot = execution.PublicationSnapshot
	}
	r, err := readVerification(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrObservationUnavailable) {
			return unavailable()
		}
		return reject()
	}
	p, config, profile, err := load()
	if err != nil {
		return unavailable()
	}
	if !workspacebundle.ValidDigest(profile) {
		return reject()
	}
	var receipt authority.Receipt
	if authority.DecodeStrict(r.Receipt, &receipt) != nil || authority.Digest(r.Payload) != r.PayloadDigest {
		return reject()
	}
	c, err := verification.Verify(p, receipt, r.Payload, time.Now())
	if err != nil || c.ID() != r.ID || c.CandidateID != r.CandidateID || !time.UnixMilli(c.ExpiresAt).Equal(r.ExpiresAt) {
		return reject()
	}
	candidate, err := readCandidate(ctx, c.CandidateID)
	if errors.Is(err, store.ErrObservationUnavailable) {
		return unavailable()
	}
	if err != nil || verification.MatchLedger(c, candidate) != nil {
		return reject()
	}
	var original authority.Receipt
	if authority.DecodeStrict(candidate.Receipt, &original) != nil {
		return reject()
	}
	generated, err := execution.Verify(p, original, candidate.Payload, time.Now())
	if err != nil || generated.ProfileSHA256 != profile || generated.ID() != c.CandidateID || generated.WorkItemID != c.WorkItemID || generated.Repository != c.Repository || generated.SourceSHA != c.SourceSHA || generated.Generation != c.Generation || generated.IssueNumber != c.IssueNumber || c.ExpiresAt > generated.ExpiresAt {
		return reject()
	}
	snapshot, err := readSnapshot(ctx, db, p, config, generated)
	if errors.Is(err, store.ErrObservationUnavailable) {
		return unavailable()
	}
	_, selected := verification.SelectRecipe(c.Repository, snapshot.Issue.Body)
	if err != nil || snapshot.Revision != c.IssueRevision || !selected {
		return reject()
	}
	latest, latestConfig, latestProfile, err := load()
	if err != nil || ctx.Err() != nil {
		return unavailable()
	}
	if latestProfile != profile || latest.Revision() != p.Revision() || latestConfig.Digest() != config.Digest() || ctx.Err() != nil {
		return reject()
	}
	if _, err = verification.Verify(latest, receipt, r.Payload, time.Now()); err != nil {
		return reject()
	}
	return c, candidate, snapshot, nil
}
