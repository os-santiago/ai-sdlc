package flowconsole

import (
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/flowevent"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// Seen in a real evaluation journal: merging closes the issue, intake
// observes the closed revision, and admission rejects it. That rejection
// ends delivered work; it must not read as a refusal on every lane.
func TestAdmissionRejectionAfterMergeIsIssueClosedNotAFailure(t *testing.T) {
	base := time.Date(2026, 11, 26, 4, 0, 0, 0, time.UTC)
	item := "8f9a2c1e-3b4d-4e5f-8a6b-7c8d9e0f1a2b"
	merged := base.Add(6 * time.Minute)
	j := store.FlowJournal{
		ObservedAt: base.Add(time.Hour),
		WorkItems:  []store.FlowWorkItem{{UUID: item, Repository: "example/app", Issues: []int{258}, Status: "ready", CreatedAt: base, UpdatedAt: base}},
		Admissions: []store.FlowAdmission{
			{Repository: "example/app", Issue: 258, Decision: "accepted", Risk: "low", CreatedAt: base, UpdatedAt: base},
			{Repository: "example/app", Issue: 258, Decision: "rejected", Risk: "low", CreatedAt: merged.Add(30 * time.Second), UpdatedAt: merged.Add(30 * time.Second)},
			// A rejection of an issue whose work never merged stays a refusal.
			{Repository: "example/app", Issue: 259, Decision: "rejected", Risk: "low", CreatedAt: merged, UpdatedAt: merged},
		},
		Merges: []store.FlowVerdict{{ID: "merge-1", FlowLink: store.FlowLink{WorkItem: item, Attempt: 1, Repository: "example/app"}, State: "merged", CreatedAt: base.Add(5 * time.Minute), FinishedAt: &merged}},
	}
	var closed, refused int
	for _, e := range Derive(j, DeriveOptions{}) {
		if e.Stage != "admission" {
			continue
		}
		switch {
		case e.Issue == 258 && e.Step == "issue-closed" && e.Kind == flowevent.KindCompleted && e.Diagnostic == "" && e.Attributes["decision"] == "rejected":
			closed++
		case e.Issue == 258 && e.Kind == flowevent.KindFailed:
			t.Fatal("closure after merge derived as a refusal")
		case e.Issue == 259 && e.Kind == flowevent.KindFailed && e.Diagnostic == "rejected":
			refused++
		}
	}
	if closed != 1 || refused != 1 {
		t.Fatal("closed", closed, "refused", refused)
	}
}
