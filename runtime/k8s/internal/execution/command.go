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
