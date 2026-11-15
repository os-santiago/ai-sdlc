package verification

import (
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func TestLedgerRejectsSubstitutedCandidate(t *testing.T) {
	source, raw := recipeFixture(t)
	d, _ := candidate.Decode(raw)
	c := Command{CandidateID: "execution-" + strings.Repeat("c", 64), WorkItemID: "11111111-1111-4111-8111-111111111111", Repository: "os-santiago/homedir", SourceSHA: source.SourceSHA, DeltaSHA256: workspacebundle.Digest(raw), CandidateTreeSHA256: d.CandidateTreeSHA256}
	good := store.VerificationCandidate{WorkItem: c.WorkItemID, Repository: c.Repository, Delta: raw, Record: store.CandidateRecord{RequestID: c.CandidateID, SourceSHA: c.SourceSHA, SourceTreeSHA256: d.SourceTreeSHA256, CandidateTreeSHA256: d.CandidateTreeSHA256, DeltaSHA256: c.DeltaSHA256, Changes: len(d.Changes)}}
	if MatchLedger(c, good) != nil {
		t.Fatal("matching ledger rejected")
	}
	for _, kind := range []string{"request", "work", "repository", "source", "source-tree", "candidate-tree", "digest", "changes", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			r := good
			switch kind {
			case "request":
				r.Record.RequestID = "execution-" + strings.Repeat("d", 64)
			case "work":
				r.WorkItem = "22222222-2222-4222-8222-222222222222"
			case "repository":
				r.Repository = "other/project"
			case "source":
				r.Record.SourceSHA = strings.Repeat("b", 40)
			case "source-tree":
				r.Record.SourceTreeSHA256 = strings.Repeat("e", 64)
			case "candidate-tree":
				r.Record.CandidateTreeSHA256 = strings.Repeat("e", 64)
			case "digest":
				r.Record.DeltaSHA256 = strings.Repeat("e", 64)
			case "changes":
				r.Record.Changes++
			case "bytes":
				r.Delta = append(append([]byte{}, raw...), byte('\n'))
			}
			if MatchLedger(c, r) == nil {
				t.Fatal("substituted ledger accepted")
			}
		})
	}
}
