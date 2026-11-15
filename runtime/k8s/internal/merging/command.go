// Package merging defines fresh bounded merge authority. A historical
// publication fact or a forge observation alone never authorizes a mutation.
package merging

import (
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

const Version = "ai-sdlc.merge-command/v1"
const MaxAge = 2 * time.Minute

var ErrMerge = errors.New("fresh merge authority denied")
var publicationID = regexp.MustCompile(`^publication-[a-f0-9]{64}$`)
var branch = regexp.MustCompile(`^ai-sdlc/issue-[1-9][0-9]{0,9}-[a-f0-9]{32}$`)

type Command struct {
	Version                 string `json:"version"`
	PublicationID           string `json:"publicationID"`
	PublicationRecordSHA256 string `json:"publicationRecordSHA256"`
	GateSHA256              string `json:"gateSHA256"`
	Repository              string `json:"repository"`
	IssueNumber             int    `json:"issueNumber"`
	PullRequest             int    `json:"pullRequest"`
	HeadSHA                 string `json:"headSHA"`
	BaseSHA                 string `json:"baseSHA"`
	TreeSHA                 string `json:"treeSHA"`
	Branch                  string `json:"branch"`
	AuthorityRevision       string `json:"authorityRevision"`
	IssuedAt                int64  `json:"issuedAt"`
	ExpiresAt               int64  `json:"expiresAt"`
}

func (c Command) ID() string { raw, _ := json.Marshal(c); return "merge-" + authority.Digest(raw) }
func Verify(p authority.Policy, receipt authority.Receipt, raw []byte, now time.Time) (Command, error) {
	var c Command
	if len(raw) == 0 || len(raw) > 16384 || authority.DecodeStrict(raw, &c) != nil || c.Version != Version || c.Repository != "os-santiago/homedir" || p.Repository != c.Repository || p.Autonomy != "auto-merge-low" || c.AuthorityRevision != p.Revision() || !publicationID.MatchString(c.PublicationID) || c.IssueNumber < 1 || c.IssueNumber > 2147483647 || c.PullRequest < 1 || c.PullRequest > 2147483647 || !branch.MatchString(c.Branch) || receipt.ID != c.ID() {
		return Command{}, ErrMerge
	}
	for _, sha := range []string{c.HeadSHA, c.BaseSHA, c.TreeSHA} {
		if !workspacebundle.ValidSourceSHA(sha) {
			return Command{}, ErrMerge
		}
	}
	for _, hash := range []string{c.PublicationRecordSHA256, c.GateSHA256, c.AuthorityRevision} {
		if !workspacebundle.ValidDigest(hash) {
			return Command{}, ErrMerge
		}
	}
	issued, expires := time.UnixMilli(c.IssuedAt), time.UnixMilli(c.ExpiresAt)
	if c.IssuedAt <= 0 || issued.After(now.Add(5*time.Second)) || !expires.After(now) || !expires.After(issued) || expires.Sub(issued) > MaxAge || expires.After(now.Add(MaxAge+5*time.Second)) {
		return Command{}, ErrMerge
	}
	if p.VerifyReceipt(receipt, "release-manager", "merge-publication", raw) != nil {
		return Command{}, ErrMerge
	}
	return c, nil
}
