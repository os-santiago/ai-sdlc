package repair

import (
	"encoding/json"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Stop is a durable rejection fact. Only its transport expires: a late relay
// may close failed work, but this fact never grants execution or new capacity.
type Stop struct {
	Version          string `json:"version"`
	Repository       string `json:"repository"`
	WorkItemID       string `json:"workItemID"`
	ReviewID         string `json:"reviewID"`
	FeedbackSHA256   string `json:"feedbackSHA256"`
	Reason           string `json:"reason"`
	UnstartedChildID string `json:"unstartedChildID,omitempty"`
}

func TerminalReason(reason string) bool {
	switch reason {
	case "not-authorized", "expired", "exhausted", "stale", "terminal-rejection":
		return true
	}
	return false
}
func (s Stop) ID() string { raw, _ := json.Marshal(s); return "rejection-" + authority.Digest(raw) }
func VerifyStop(p authority.Policy, receipt authority.Receipt, raw []byte) (Stop, error) {
	var s Stop
	if len(raw) < 1 || len(raw) > 4096 || authority.DecodeStrict(raw, &s) != nil || s.Version != "ai-sdlc.rejected-work/v1" || s.Repository != p.Repository || !workID.MatchString(s.WorkItemID) || !reviewID.MatchString(s.ReviewID) || !workspacebundle.ValidDigest(s.FeedbackSHA256) || !TerminalReason(s.Reason) ||
		(s.UnstartedChildID != "" && (!candidateID.MatchString(s.UnstartedChildID) || s.Reason != "expired")) || receipt.ID != s.ID() || p.VerifyReceipt(receipt, "orchestrator", "stop-rejected-work", raw) != nil {
		return Stop{}, ErrRepair
	}
	return s, nil
}
