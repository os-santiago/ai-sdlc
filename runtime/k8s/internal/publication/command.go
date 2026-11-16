// Package publication defines the signed outerloop handoff. These pure contracts
// neither publish code nor replace authoritative ledger/current-issue checks.
package publication

import (
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

const Version = "ai-sdlc.publication-command/v1"

var ErrPublication = errors.New("publication evidence denied")
var verificationID = regexp.MustCompile(`^verification-[a-f0-9]{64}$`)
var candidateID = regexp.MustCompile(`^execution-[a-f0-9]{64}$`)
var workID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

// No branch, URL, shell text, commit identity, review waiver, merge or deployment
// instruction is accepted. The release manager derives its finite publication
// plan from independently read immutable evidence.
type Command struct {
	Version                 string `json:"version"`
	VerificationID          string `json:"verificationId"`
	CandidateID             string `json:"candidateId"`
	WorkItemID              string `json:"workItemId"`
	Repository              string `json:"repository"`
	IssueNumber             int    `json:"issueNumber"`
	Generation              int64  `json:"generation"`
	IssueRevision           string `json:"issueRevision"`
	SourceSHA               string `json:"sourceSHA"`
	DeltaSHA256             string `json:"deltaSHA256"`
	CandidateTreeSHA256     string `json:"candidateTreeSHA256"`
	BehaviorProofSHA256     string `json:"behaviorProofSHA256"`
	IndependentReviewSHA256 string `json:"independentReviewSHA256"`
	ReviewerProfileSHA256   string `json:"reviewerProfileSHA256"`
	AuthorityRevision       string `json:"authorityRevision"`
	ExpiresAt               int64  `json:"expiresAt"`
}

func (c Command) ID() string {
	raw, _ := json.Marshal(c)
	return "publication-" + authority.Digest(raw)
}

func Verify(p authority.Policy, receipt authority.Receipt, raw []byte, now time.Time) (Command, error) {
	var c Command
	if len(raw) == 0 || len(raw) > 16384 || authority.DecodeStrict(raw, &c) != nil {
		return Command{}, ErrPublication
	}
	if c.Version != Version || c.Repository != p.Repository || !verificationID.MatchString(c.VerificationID) || !candidateID.MatchString(c.CandidateID) || !workID.MatchString(c.WorkItemID) || c.IssueNumber < 1 || c.IssueNumber > 2147483647 || c.Generation < 1 || !workspacebundle.ValidSourceSHA(c.SourceSHA) || c.AuthorityRevision != p.Revision() || receipt.ID != c.ID() || !time.UnixMilli(c.ExpiresAt).After(now) || time.UnixMilli(c.ExpiresAt).After(now.Add(verification.MaxCommandAge)) {
		return Command{}, ErrPublication
	}
	for _, digest := range []string{c.IssueRevision, c.DeltaSHA256, c.CandidateTreeSHA256, c.BehaviorProofSHA256, c.IndependentReviewSHA256, c.ReviewerProfileSHA256, c.AuthorityRevision} {
		if !workspacebundle.ValidDigest(digest) {
			return Command{}, ErrPublication
		}
	}
	if p.VerifyReceipt(receipt, "release-manager", "publish-candidate", raw) != nil {
		return Command{}, ErrPublication
	}
	return c, nil
}

// MatchVerification only checks immutable identity/expiry binding. Its caller
// must verify the original worker receipt, current issue/authority and the
// persisted independent behavior/review evidence before performing any effect.
func MatchVerification(c Command, v verification.Command) error {
	if c.VerificationID != v.ID() || c.CandidateID != v.CandidateID || c.WorkItemID != v.WorkItemID || c.Repository != v.Repository || c.IssueNumber != v.IssueNumber || c.Generation != v.Generation || c.IssueRevision != v.IssueRevision || c.SourceSHA != v.SourceSHA || c.DeltaSHA256 != v.DeltaSHA256 || c.CandidateTreeSHA256 != v.CandidateTreeSHA256 || c.AuthorityRevision != v.AuthorityRevision || c.ExpiresAt > v.ExpiresAt || !verification.ValidRecipe(v.Repository, v.Recipe) || v.RecipeRevision != verification.RevisionForRecipe(v.Recipe) {
		return ErrPublication
	}
	return nil
}
