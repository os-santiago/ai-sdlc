package repair

import (
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

const Version = "ai-sdlc.repair-command/v1"

var ErrRepair = errors.New("repair authority denied")
var workID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

// Command conveys references only. Review prose stays in the private ledger;
// neither the reviewer nor this command can allocate fresh generation budget.
type Command struct {
	Version           string    `json:"version"`
	Repository        string    `json:"repository"`
	WorkItemID        string    `json:"workItemID"`
	AuthorityRevision string    `json:"authorityRevision"`
	Reference         Reference `json:"reference"`
	ExpiresAt         int64     `json:"expiresAt"`
}

func (c Command) ID() string {
	raw, _ := json.Marshal(c)
	return "repair-" + authority.Digest(raw)
}

func Verify(p authority.Policy, receipt authority.Receipt, raw []byte, now time.Time) (Command, error) {
	var c Command
	if len(raw) < 1 || len(raw) > 16384 || authority.DecodeStrict(raw, &c) != nil || c.Version != Version ||
		!workID.MatchString(c.WorkItemID) || c.Repository != p.Repository || c.AuthorityRevision != p.Revision() ||
		!c.Reference.Valid() || receipt.ID != c.ID() || !time.UnixMilli(c.ExpiresAt).After(now) ||
		time.UnixMilli(c.ExpiresAt).After(now.Add(time.Hour)) || p.VerifyReceipt(receipt, "orchestrator", "repair-candidate", raw) != nil {
		return Command{}, ErrRepair
	}
	return c, nil
}
