// Package execution binds a durable worker request to signed orchestrator
// authority. It never creates Pods or executes generated code.
package execution

import (
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repair"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

const Version = "ai-sdlc.execution-command/v1"
const MaxCommandBytes = 16 * 1024

var ErrAuthority = errors.New("execution authority denied")
var uuid = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var admissionID = regexp.MustCompile(`^admission-issue-[a-f0-9]{64}$`)

type Command struct {
	Version           string            `json:"version"`
	WorkItemID        string            `json:"workItemId"`
	Attempt           int               `json:"attempt"`
	Repository        string            `json:"repository"`
	IssueNumber       int               `json:"issueNumber"`
	Generation        int64             `json:"generation"`
	AdmissionID       string            `json:"admissionId"`
	SourceSHA         string            `json:"sourceSHA"`
	ProfileSHA256     string            `json:"profileSHA256"`
	AuthorityRevision string            `json:"authorityRevision"`
	ExpiresAt         int64             `json:"expiresAt"`
	Repair            *repair.Reference `json:"repair,omitempty"`
}

// BaseGenerationSeconds is the generation budget the original 20-minute
// command lifetime was sized for: 600 s of generation inside the 720 s
// lifecycle claim (maxSeconds + 120 s), leaving 480 s for queueing,
// preparation and the verification handoff, which inherits this expiry.
const BaseGenerationSeconds = 600

// CommandLifetime is the signed execution command's lifetime, and the worker's
// per-poll bound, for a reviewed generation budget. A budget above 600 s
// extends both by the same number of seconds (at most 25 minutes at the 900 s
// maximum), so a longer generation keeps the original 480 s for preparation
// and handoff. The default 600 s budget keeps exactly 20 minutes.
func CommandLifetime(maxSeconds int) time.Duration {
	return 20*time.Minute + time.Duration(max(0, min(maxSeconds, 900)-BaseGenerationSeconds))*time.Second
}

func (c Command) ID() string {
	raw, _ := json.Marshal(c)
	return "execution-" + authority.Digest(raw)
}

func Verify(p authority.Policy, receipt authority.Receipt, raw []byte, now time.Time) (Command, error) {
	var c Command
	if len(raw) == 0 || len(raw) > MaxCommandBytes || authority.DecodeStrict(raw, &c) != nil {
		return c, ErrAuthority
	}
	if c.Version != Version || !uuid.MatchString(c.WorkItemID) || c.Attempt < 1 || c.Attempt > 16 ||
		c.Repository != p.Repository || c.IssueNumber < 1 || c.IssueNumber > 2147483647 || c.Generation < 1 ||
		!admissionID.MatchString(c.AdmissionID) || !workspacebundle.ValidSourceSHA(c.SourceSHA) ||
		!workspacebundle.ValidDigest(c.ProfileSHA256) || c.AuthorityRevision != p.Revision() ||
		receipt.ID != c.ID() || !time.UnixMilli(c.ExpiresAt).After(now) ||
		time.UnixMilli(c.ExpiresAt).After(now.Add(time.Hour)) ||
		p.VerifyReceipt(receipt, "worker", "execute-work", raw) != nil {
		return Command{}, ErrAuthority
	}
	if (c.Repair == nil && c.Attempt != 1) || (c.Repair != nil && (c.Attempt != 2 || !c.Repair.Valid() || c.Repair.ParentCandidateID == c.ID())) {
		return Command{}, ErrAuthority
	}
	return c, nil
}
