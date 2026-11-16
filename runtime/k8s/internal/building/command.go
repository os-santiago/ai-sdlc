// Package building defines bounded outerloop authority. Merged history is an
// input fact, never permission to build or publish a new artifact.
package building

import (
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

const Version = "ai-sdlc.build-command/v1"
const Recipe = "ai-sdlc.saferus-go-static/v1"
const MaxAge = 20 * time.Minute

var ErrBuild = errors.New("fresh build authority denied")
var mergeID = regexp.MustCompile(`^merge-[a-f0-9]{64}$`)

// Command contains only references to verified facts and reviewed installation
// configuration. It contains no command, URL, credentials, YAML or image choice.
// ProfileSHA256 binds pinned toolchain/publisher, slot pool, network and budget.
type Command struct {
	Version             string `json:"version"`
	MergeID             string `json:"mergeID"`
	MergeRecordSHA256   string `json:"mergeRecordSHA256"`
	Repository          string `json:"repository"`
	Environment         string `json:"environment"`
	IssueNumber         int    `json:"issueNumber"`
	IssueRevision       string `json:"issueRevision"`
	Generation          int64  `json:"generation"`
	SourceSHA           string `json:"sourceSHA"`
	TreeSHA             string `json:"treeSHA"`
	Recipe              string `json:"recipe"`
	ProfileSHA256       string `json:"profileSHA256"`
	IntakeConfigSHA256  string `json:"intakeConfigSHA256"`
	InnerPolicyRevision string `json:"innerPolicyRevision"`
	OuterPolicyRevision string `json:"outerPolicyRevision"`
	IssuedAt            int64  `json:"issuedAt"`
	ExpiresAt           int64  `json:"expiresAt"`
}

func (c Command) ID() string { raw, _ := json.Marshal(c); return "build-" + authority.Digest(raw) }

func Verify(inner, outer authority.Policy, receipt authority.Receipt, raw []byte, now time.Time) (Command, error) {
	var c Command
	if len(raw) == 0 || len(raw) > 16384 || authority.DecodeStrict(raw, &c) != nil ||
		authority.ValidateLoopPolicies(inner, outer) != nil || inner.Freeze || outer.Freeze ||
		c.Version != Version || c.Repository != outer.Repository || c.Environment != outer.Environment ||
		c.Recipe != Recipe || !mergeID.MatchString(c.MergeID) || c.IssueNumber < 1 || c.IssueNumber > 2147483647 ||
		c.Generation < 1 || c.InnerPolicyRevision != inner.Revision() || c.OuterPolicyRevision != outer.Revision() || receipt.ID != c.ID() {
		return Command{}, ErrBuild
	}
	for _, h := range []string{c.MergeRecordSHA256, c.IssueRevision, c.ProfileSHA256, c.IntakeConfigSHA256, c.InnerPolicyRevision, c.OuterPolicyRevision} {
		if !workspacebundle.ValidDigest(h) {
			return Command{}, ErrBuild
		}
	}
	for _, h := range []string{c.SourceSHA, c.TreeSHA} {
		if !workspacebundle.ValidSourceSHA(h) {
			return Command{}, ErrBuild
		}
	}
	issued, expires := time.UnixMilli(c.IssuedAt), time.UnixMilli(c.ExpiresAt)
	if c.IssuedAt <= 0 || issued.After(now.Add(5*time.Second)) || !expires.After(now) || !expires.After(issued) || expires.Sub(issued) > MaxAge || expires.After(now.Add(MaxAge+5*time.Second)) {
		return Command{}, ErrBuild
	}
	if outer.VerifyReceipt(receipt, "build-controller", "build-merged", raw) != nil {
		return Command{}, ErrBuild
	}
	return c, nil
}
