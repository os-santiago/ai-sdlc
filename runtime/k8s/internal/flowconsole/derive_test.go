package flowconsole

import (
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/flowevent"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// Seen in a real evaluation journal: verification's effects (Kubernetes
// object creations) were all acknowledged about 6 s after its claim, and
// the review was created from its passed verdict about 55 s later. The
// verdict time is bounded by the next stage's creation, not the last effect.
func TestPassedVerdictEndsAtTheNextStagesCreation(t *testing.T) {
	base := time.Date(2026, 11, 26, 4, 11, 37, 0, time.UTC)
	item := "8f9a2c1e-3b4d-4e5f-8a6b-7c8d9e0f1a2b"
	link := store.FlowLink{WorkItem: item, Attempt: 1, Repository: "example/app"}
	done := base.Add(6 * time.Second)
	j := store.FlowJournal{
		ObservedAt: base.Add(time.Hour),
		Verifications: []store.FlowVerdict{
			{ID: "verification-1", FlowLink: link, State: "passed", Cleanup: "complete", CreatedAt: base, Candidate: "execution-1"},
			{ID: "verification-2", FlowLink: link, State: "failed", Reason: "comparison-failed", Cleanup: "complete", CreatedAt: base, Candidate: "execution-2"},
		},
		VerificationEffects: []store.FlowEffect{
			{Parent: "verification-1", Kind: "tests", CreatedAt: base.Add(time.Second), CompletedAt: &done},
			{Parent: "verification-2", Kind: "tests", CreatedAt: base.Add(time.Second), CompletedAt: &done},
		},
		Reviews: []store.FlowVerdict{
			{ID: "review-1", FlowLink: link, State: "queued", Cleanup: "pending", CreatedAt: base.Add(60 * time.Second), Candidate: "execution-1"},
			// A later review of another candidate must not stretch verification-2.
			{ID: "review-9", FlowLink: link, State: "queued", Cleanup: "pending", CreatedAt: base.Add(90 * time.Second), Candidate: "execution-9"},
		},
	}
	for _, e := range Derive(j, DeriveOptions{}) {
		if e.Stage != "verification" || e.Step != "verdict" {
			continue
		}
		switch e.Kind {
		case flowevent.KindCompleted:
			if !e.Time.Equal(base.Add(60*time.Second)) || e.DurationMs != 59000 {
				t.Fatal("passed verdict not bounded by the review's creation", e.Time, e.DurationMs)
			}
		case flowevent.KindFailed:
			if !e.Time.Equal(done) {
				t.Fatal("a refusal has no successor and keeps its last effect", e.Time)
			}
		}
	}
}

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
