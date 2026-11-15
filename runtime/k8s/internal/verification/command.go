package verification

import (
	"encoding/json"
	"regexp"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

const MaxCommandAge = 30 * time.Minute

const CommandVersion = "ai-sdlc.verification-command/v1"

var candidateID = regexp.MustCompile(`^execution-[a-f0-9]{64}$`)
var workID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

// Command references immutable bytes in the candidate ledger. It cannot carry
// tests, a shell command, a URL, an audit verdict or publication authority.
type Command struct {
	Version             string `json:"version"`
	CandidateID         string `json:"candidateId"`
	WorkItemID          string `json:"workItemId"`
	Repository          string `json:"repository"`
	IssueNumber         int    `json:"issueNumber"`
	Generation          int64  `json:"generation"`
	IssueRevision       string `json:"issueRevision"`
	SourceSHA           string `json:"sourceSHA"`
	DeltaSHA256         string `json:"deltaSHA256"`
	CandidateTreeSHA256 string `json:"candidateTreeSHA256"`
	Recipe              string `json:"recipe"`
	RecipeRevision      string `json:"recipeRevision"`
	AuthorityRevision   string `json:"authorityRevision"`
	ExpiresAt           int64  `json:"expiresAt"`
}

func (c Command) ID() string {
	raw, _ := json.Marshal(c)
	return "verification-" + authority.Digest(raw)
}
func Verify(p authority.Policy, receipt authority.Receipt, raw []byte, now time.Time) (Command, error) {
	var c Command
	if len(raw) == 0 || len(raw) > 16384 || authority.DecodeStrict(raw, &c) != nil {
		return c, ErrVerification
	}
	if c.Version != CommandVersion || !candidateID.MatchString(c.CandidateID) || !workID.MatchString(c.WorkItemID) || c.Repository != p.Repository || c.IssueNumber < 1 || c.IssueNumber > 2147483647 || c.Generation < 1 || !workspacebundle.ValidDigest(c.IssueRevision) || !workspacebundle.ValidSourceSHA(c.SourceSHA) || !workspacebundle.ValidDigest(c.DeltaSHA256) || !workspacebundle.ValidDigest(c.CandidateTreeSHA256) || c.Recipe != LocaleRecipe || c.RecipeRevision != RecipeRevision() || c.AuthorityRevision != p.Revision() || receipt.ID != c.ID() || !time.UnixMilli(c.ExpiresAt).After(now) || time.UnixMilli(c.ExpiresAt).After(now.Add(MaxCommandAge)) || p.VerifyReceipt(receipt, "verifier", "verify-artifact", raw) != nil {
		return Command{}, ErrVerification
	}
	return c, nil
}
