// Package publicationflow joins the signed verifier handoff, current issue
// authority and an exclusive publication journal. It has no external input.
package publicationflow

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publication"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	"time"
)

type ReviewPolicy struct{ Profile, Configuration string }
type ReviewLoader func() (ReviewPolicy, error)

// This is the strict persisted v1 proof schema, not a model response or a caller
// assertion. The review store commits it only after journal/effect validation.
type reviewProof struct {
	Version               string `json:"version"`
	ReviewID              string `json:"reviewID"`
	VerificationID        string `json:"verificationID"`
	CandidateID           string `json:"candidateID"`
	BehaviorSHA256        string `json:"behaviorSHA256"`
	ProfileSHA256         string `json:"profileSHA256"`
	ConfigurationSHA256   string `json:"configurationSHA256"`
	AssignmentSHA256      string `json:"assignmentSHA256"`
	CandidateTreeSHA256   string `json:"candidateTreeSHA256"`
	ModelEvidenceSHA256   string `json:"modelEvidenceSHA256"`
	Verdict               string `json:"verdict"`
	SourceUnchanged       bool   `json:"sourceUnchanged"`
	PublicationAuthorized bool   `json:"publicationAuthorized"`
}

func matchProofs(v verification.Command, r store.PassedReview, expected ReviewPolicy) (publication.Command, error) {
	return matchProofsAt(v, r, expected, time.Now())
}

func matchProofsAt(v verification.Command, r store.PassedReview, expected ReviewPolicy, observed time.Time) (publication.Command, error) {
	reject := func() (publication.Command, error) { return publication.Command{}, publication.ErrPublication }
	if !workspacebundle.ValidDigest(expected.Profile) || !workspacebundle.ValidDigest(expected.Configuration) || len(r.Proof) > 32768 || len(r.Behavior) > 16384 {
		return reject()
	}
	var b verificationflow.BehaviorProof
	if authority.DecodeStrict(r.Behavior, &b) != nil || b.Version != "ai-sdlc.behavior-proof/v1" || b.Command != v || !b.BehaviorVerified || b.PublicationAuthorized || len(b.ResourceUIDs) != 5 {
		return reject()
	}
	for _, key := range []string{"config", "baseline", "candidate", "reference", "tests"} {
		if b.ResourceUIDs[key] == "" {
			return reject()
		}
	}
	var p reviewProof
	if authority.DecodeStrict(r.Proof, &p) != nil || p.Version != "ai-sdlc.independent-review-proof/v1" || p.Verdict != "approve" || !p.SourceUnchanged || p.PublicationAuthorized || p.ReviewID != r.Run.ID || p.VerificationID != v.ID() || p.CandidateID != v.CandidateID || p.BehaviorSHA256 != authority.Digest(r.Behavior) || p.ProfileSHA256 != expected.Profile || p.ConfigurationSHA256 != expected.Configuration || p.AssignmentSHA256 != r.Run.AssignmentDigest || p.CandidateTreeSHA256 != v.CandidateTreeSHA256 || !workspacebundle.ValidDigest(p.ModelEvidenceSHA256) {
		return reject()
	}
	if r.Run.VerificationID != v.ID() || r.Run.CandidateID != v.CandidateID || r.Run.WorkItem != v.WorkItemID || r.Run.BehaviorDigest != p.BehaviorSHA256 || r.Run.ProfileDigest != p.ProfileSHA256 || r.Run.ConfigurationDigest != p.ConfigurationSHA256 || r.Run.ID != store.ReviewID(v.ID(), p.BehaviorSHA256, p.ProfileSHA256, p.ConfigurationSHA256, p.AssignmentSHA256) || !r.Run.ExpiresAt.After(observed) || r.Run.ExpiresAt.After(time.UnixMilli(v.ExpiresAt)) {
		return reject()
	}
	return publication.Command{Version: publication.Version, VerificationID: v.ID(), CandidateID: v.CandidateID, WorkItemID: v.WorkItemID, Repository: v.Repository, IssueNumber: v.IssueNumber, Generation: v.Generation, IssueRevision: v.IssueRevision, SourceSHA: v.SourceSHA, DeltaSHA256: v.DeltaSHA256, CandidateTreeSHA256: v.CandidateTreeSHA256, BehaviorProofSHA256: p.BehaviorSHA256, IndependentReviewSHA256: authority.Digest(r.Proof), ReviewerProfileSHA256: p.ProfileSHA256, AuthorityRevision: v.AuthorityRevision, ExpiresAt: r.Run.ExpiresAt.UnixMilli()}, nil
}

// CurrentProof is repeated immediately before and after every external effect.
// It reads immutable generation/review evidence using release-manager identity,
// then verifies the latest issue observation and the current installed policies.
func CurrentProof(ctx context.Context, db *store.DB, policy verificationflow.PolicyLoader, review ReviewLoader, id string) (publication.Command, store.VerificationCandidate, error) {
	reject := func() (publication.Command, store.VerificationCandidate, error) {
		return publication.Command{}, store.VerificationCandidate{}, publication.ErrPublication
	}
	if db == nil || policy == nil || review == nil {
		return reject()
	}
	r, e := db.ReadPublication(ctx, id)
	if e != nil {
		return reject()
	}
	p, _, _, e := policy()
	if e != nil {
		return reject()
	}
	var receipt authority.Receipt
	if authority.DecodeStrict(r.Receipt, &receipt) != nil || authority.Digest(r.Payload) != r.PayloadDigest {
		return reject()
	}
	c, e := publication.Verify(p, receipt, r.Payload, time.Now())
	if e != nil || c.ID() != id || c.VerificationID != r.VerificationID || c.CandidateID != r.CandidateID || c.Repository != r.Repository || !time.UnixMilli(c.ExpiresAt).Equal(r.ExpiresAt) {
		return reject()
	}
	expected, e := review()
	if e != nil {
		return reject()
	}
	v, candidate, _, e := verificationflow.CurrentPublicationProof(ctx, db, policy, c.VerificationID)
	if e != nil || publication.MatchVerification(c, v) != nil {
		return reject()
	}
	passed, e := db.ReadPublicationReview(ctx, c.VerificationID)
	if e != nil || passed.Run.ID != r.ReviewID {
		return reject()
	}
	matched, e := matchProofs(v, passed, expected)
	if e != nil || matched != c {
		return reject()
	}
	latest, _, _, e := policy()
	if e != nil || latest.Revision() != p.Revision() {
		return reject()
	}
	latestReview, e := review()
	if e != nil || latestReview != expected || ctx.Err() != nil {
		return reject()
	}
	if _, e = publication.Verify(latest, receipt, r.Payload, time.Now()); e != nil {
		return reject()
	}
	return c, candidate, nil
}
