package verification

import (
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// MatchLedger consumes a record fetched by the independent verifier database
// role. A signed name/digest tuple alone cannot substitute an unrelated row.
// Current signed execution/issue evidence remains a separate controller gate.
func MatchLedger(c Command, r store.VerificationCandidate) error {
	if r.Record.RequestID != c.CandidateID || r.WorkItem != c.WorkItemID || r.Repository != c.Repository || r.Record.SourceSHA != c.SourceSHA || r.Record.DeltaSHA256 != c.DeltaSHA256 || r.Record.CandidateTreeSHA256 != c.CandidateTreeSHA256 || workspacebundle.Digest(r.Delta) != c.DeltaSHA256 {
		return ErrVerification
	}
	d, err := candidate.Decode(r.Delta)
	if err != nil || d.SourceSHA != c.SourceSHA || d.SourceTreeSHA256 != r.Record.SourceTreeSHA256 || d.CandidateTreeSHA256 != c.CandidateTreeSHA256 || len(d.Changes) != r.Record.Changes {
		return ErrVerification
	}
	return nil
}
