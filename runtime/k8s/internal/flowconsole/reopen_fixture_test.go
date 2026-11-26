package flowconsole

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/flowevent"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

var updateFixtures = flag.Bool("update-fixtures", false, "rewrite the derived flow-console fixtures")

const reopenFixture = "../../components/flow-console/fixtures/reopened-issue.jsonl"

// reopenedIssueJournal is one issue delivered twice: merge #1 and its
// closure (issue-closed plus one refusal of the closed issue's next
// revision), a reopen refused at high risk, a second reopen accepted, and
// merge #2 with its own closure.
func reopenedIssueJournal() store.FlowJournal {
	base := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	at := func(m float64) time.Time { return base.Add(time.Duration(m * float64(time.Minute))) }
	ptr := func(m float64) *time.Time { v := at(m); return &v }
	const repo, issue = "example/app", 301
	j := store.FlowJournal{ObservedAt: at(45)}
	intake := func(m float64, generation int64) {
		j.Intake = append(j.Intake, store.FlowIntake{Repository: repo, Issue: issue, Generation: generation, ObservedAt: at(m)})
	}
	admission := func(m float64, decision, risk string) {
		j.Admissions = append(j.Admissions, store.FlowAdmission{Repository: repo, Issue: issue, Decision: decision, Risk: risk, CreatedAt: at(m - 0.05), UpdatedAt: at(m)})
	}
	delivery := func(o float64, item string) {
		link := store.FlowLink{WorkItem: item, Attempt: 1, Repository: repo}
		changes, calls := 2, 5
		exec, ver, rev, pub, mrg := "execution-"+item[:8], "verification-"+item[:8], "review-"+item[:8], "publication-"+item[:8], "merge-"+item[:8]
		j.WorkItems = append(j.WorkItems, store.FlowWorkItem{UUID: item, Repository: repo, Issues: []int{issue}, Status: "ready", CreatedAt: at(o + 1), UpdatedAt: at(o + 1)})
		j.Executions = append(j.Executions, store.FlowExecution{ID: exec, FlowLink: link, State: "stopped", StopReason: "candidate-captured",
			CreatedAt: at(o + 1.2), UpdatedAt: at(o + 5), ExpiresAt: at(o + 60), LifecycleAt: ptr(o + 1.5), CandidateAt: ptr(o + 5), Changes: &changes, ModelCalls: &calls})
		j.ExecutionEffects = append(j.ExecutionEffects, store.FlowEffect{Parent: exec, Kind: "pod", CreatedAt: at(o + 1.5), CompletedAt: ptr(o + 1.6)})
		j.Verifications = append(j.Verifications, store.FlowVerdict{ID: ver, FlowLink: link, State: "passed", Cleanup: "complete", CreatedAt: at(o + 5.1), ExpiresAt: at(o + 60), Candidate: exec})
		j.VerificationEffects = append(j.VerificationEffects, store.FlowEffect{Parent: ver, Kind: "tests", CreatedAt: at(o + 5.3), CompletedAt: ptr(o + 5.5)})
		j.Reviews = append(j.Reviews, store.FlowVerdict{ID: rev, FlowLink: link, State: "passed", Cleanup: "complete", CreatedAt: at(o + 6), ExpiresAt: at(o + 60), Candidate: exec})
		j.ReviewEffects = append(j.ReviewEffects, store.FlowEffect{Parent: rev, Kind: "pod", CreatedAt: at(o + 6.2), CompletedAt: ptr(o + 6.9)})
		j.Publications = append(j.Publications, store.FlowVerdict{ID: pub, FlowLink: link, State: "published", CreatedAt: at(o + 7), ExpiresAt: at(o + 60), CommitTime: ptr(o + 7.1), FinishedAt: ptr(o + 7.4), Candidate: exec})
		j.PublicationEffects = append(j.PublicationEffects, store.FlowEffect{Parent: pub, Kind: "pull-request", CreatedAt: at(o + 7.2), CompletedAt: ptr(o + 7.3)})
		j.Merges = append(j.Merges, store.FlowVerdict{ID: mrg, FlowLink: link, State: "merged", CreatedAt: at(o + 7.6), ExpiresAt: at(o + 60), FinishedAt: ptr(o + 8.2), Candidate: exec})
		j.MergeEffects = append(j.MergeEffects, store.FlowEffect{Parent: mrg, Kind: "merge", CreatedAt: at(o + 8), CompletedAt: ptr(o + 8.1)})
	}
	intake(0, 1)
	admission(0.5, "accepted", "low")
	delivery(0, "1d2c3b4a-5e6f-4a7b-8c9d-0e1f2a3b4c5d")
	intake(8.5, 2) // merge #1 closed the issue
	admission(8.6, "rejected", "low")
	intake(9, 3) // the merge's own comment: refused again
	admission(9.1, "rejected", "low")
	intake(20, 4) // reopened, refused at high risk
	admission(20.3, "rejected", "high")
	intake(30, 5) // reopened again and accepted
	admission(30.4, "accepted", "medium")
	delivery(30, "6e7f8a9b-0c1d-4e2f-9a3b-4c5d6e7f8a9b")
	intake(38.5, 6) // merge #2 closed the issue
	admission(38.6, "rejected", "medium")
	return j
}

// The reopened-issue fixture is the derivation of reopenedIssueJournal:
// each merge's closure is labelled, and the reopen's refusal is a plain
// refusal. Regenerate with -update-fixtures.
func TestReopenedIssueFixture(t *testing.T) {
	events := Derive(reopenedIssueJournal(), DeriveOptions{})
	var buf bytes.Buffer
	buf.WriteString("# ai-sdlc.flow-event/v1 events derived by TestReopenedIssueFixture: one issue delivered, reopened and refused at high risk, reopened and accepted, and delivered again\n")
	steps := map[string]int{}
	for _, e := range events {
		if err := e.Validate(); err != nil {
			t.Fatal(err)
		}
		if e.Stage == "admission" {
			steps[e.Step+"/"+string(e.Kind)+"/"+e.Attributes["risk"]]++
		}
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(append(line, '\n'))
	}
	want := map[string]int{"decision/completed/low": 1, "issue-closed/completed/low": 1, "after-issue-closed/failed/low": 1,
		"decision/failed/high": 1, "decision/completed/medium": 1, "issue-closed/completed/medium": 1}
	if len(steps) != len(want) {
		t.Fatal("admission steps", steps)
	}
	for k, n := range want {
		if steps[k] != n {
			t.Fatal("admission steps", steps)
		}
	}
	if *updateFixtures {
		if err := os.WriteFile(reopenFixture, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	committed, err := os.ReadFile(reopenFixture)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, buf.Bytes()) {
		t.Fatal("fixture out of date; run go test ./internal/flowconsole -run TestReopenedIssueFixture -update-fixtures")
	}
	if _, err := flowevent.Decode(bytes.NewReader(committed)); err != nil {
		t.Fatal(err)
	}
}
