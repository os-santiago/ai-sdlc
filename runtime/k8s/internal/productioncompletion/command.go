// Package productioncompletion describes an already completed production fact.
// Its authority permits recording only; it cannot renew release/deploy effects.
package productioncompletion

import (
	"encoding/json"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasecompletion"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	"regexp"
	"time"
)

var ErrProduction = errors.New("production completion denied")
var terminalID = regexp.MustCompile(`^complete-[a-f0-9]{64}$`)
var uuid = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var image = regexp.MustCompile(`^[a-z0-9][a-z0-9./:-]{0,447}@sha256:[a-f0-9]{64}$`)

const Version = "ai-sdlc.record-production-command/v1"

type Fact struct {
	TerminalID           string `json:"terminalID"`
	TerminalRecordSHA256 string `json:"terminalRecordSHA256"`
	WorkItemID           string `json:"workItemId"`
	Repository           string `json:"repository"`
	IssueNumber          int    `json:"issueNumber"`
	Generation           int64  `json:"generation"`
	IssueRevision        string `json:"issueRevision"`
	SourceSHA            string `json:"sourceSHA"`
	Image                string `json:"image"`
	ReleaseURL           string `json:"releaseURL"`
	CompletedAt          int64  `json:"completedAt"`
}
type Command struct {
	Version           string `json:"version"`
	Fact              Fact   `json:"fact"`
	AuthorityRevision string `json:"authorityRevision"`
}

func (c Command) ID() string { raw, _ := json.Marshal(c); return "production-" + authority.Digest(raw) }
func New(p authority.Policy, f Fact) (Command, error) {
	c := Command{Version, f, p.Revision()}
	return c, c.Validate(p, time.Now())
}
func (c Command) Validate(p authority.Policy, now time.Time) error {
	f := c.Fact
	if p.Validate() != nil || p.Freeze || p.Repository != "os-santiago/saferus" || p.Environment != "ai-sdlc-production" || p.Autonomy != "auto-deploy" || c.Version != Version || c.AuthorityRevision != p.Revision() || f.Repository != p.Repository || !terminalID.MatchString(f.TerminalID) || !uuid.MatchString(f.WorkItemID) || !workspacebundle.ValidDigest(f.TerminalRecordSHA256) || !workspacebundle.ValidDigest(f.IssueRevision) || !workspacebundle.ValidSourceSHA(f.SourceSHA) || !image.MatchString(f.Image) || f.IssueNumber < 1 || f.IssueNumber > 2147483647 || f.Generation < 1 || f.CompletedAt <= 0 || f.CompletedAt > now.Add(5*time.Second).UnixMilli() || !releasecompletion.ValidReleaseURL(f.ReleaseURL, releasecompletion.Plan{Repository: f.Repository, Tag: "ai-sdlc/" + f.TerminalID}) {
		return ErrProduction
	}
	return nil
}
func Verify(p authority.Policy, signed authority.Receipt, raw []byte) (Command, error) {
	var c Command
	if len(raw) == 0 || len(raw) > 8192 || authority.DecodeStrict(raw, &c) != nil || c.Validate(p, time.Now()) != nil || signed.ID != c.ID() || p.VerifyReceipt(signed, "orchestrator", "record-production", raw) != nil {
		return Command{}, ErrProduction
	}
	canonical, _ := json.Marshal(c)
	var a, b any
	if json.Unmarshal(raw, &a) != nil || json.Unmarshal(canonical, &b) != nil {
		return Command{}, ErrProduction
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if string(x) != string(y) {
		return Command{}, ErrProduction
	}
	return c, nil
}
