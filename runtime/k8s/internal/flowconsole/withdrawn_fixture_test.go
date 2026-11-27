package flowconsole

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/flowevent"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

const withdrawnFixture = "../../components/flow-console/fixtures/refused-then-withdrawn.jsonl"

// refusedThenWithdrawnJournal reproduces a live evaluation run (issue 46 of
// the Python sample, 2026-11-27, minutes from 00:15:09 UTC): two attempts,
// each refused by verification (comparison-failed). After each refusal the
// evaluation runner removed the trigger label, and admission rejected the
// new, unlabelled revision; between them it re-added the label and admission
// accepted again.
func refusedThenWithdrawnJournal() store.FlowJournal {
	base := time.Date(2026, 11, 27, 0, 15, 9, 0, time.UTC)
	at := func(s float64) time.Time { return base.Add(time.Duration(s * float64(time.Second))) }
	ptr := func(s float64) *time.Time { v := at(s); return &v }
	const repo, issue = "example/app", 46
	j := store.FlowJournal{ObservedAt: at(1200)}
	intake := func(s float64, generation int64) {
		j.Intake = append(j.Intake, store.FlowIntake{Repository: repo, Issue: issue, Generation: generation, ObservedAt: at(s)})
	}
	admission := func(s float64, decision string) {
		j.Admissions = append(j.Admissions, store.FlowAdmission{Repository: repo, Issue: issue, Decision: decision, Risk: "low", CreatedAt: at(s - 0.5), UpdatedAt: at(s)})
	}
	attempt := func(o float64, item string) {
		link := store.FlowLink{WorkItem: item, Attempt: 1, Repository: repo}
		changes, calls := 3, 7
		exec, ver := "execution-"+item[:8], "verification-"+item[:8]
		j.WorkItems = append(j.WorkItems, store.FlowWorkItem{UUID: item, Repository: repo, Issues: []int{issue}, Status: "ready", CreatedAt: at(o + 1), UpdatedAt: at(o + 1)})
		j.Executions = append(j.Executions, store.FlowExecution{ID: exec, FlowLink: link, State: "stopped", StopReason: "candidate-captured",
			CreatedAt: at(o + 2), UpdatedAt: at(o + 121), ExpiresAt: at(o + 3600), LifecycleAt: ptr(o + 5), CandidateAt: ptr(o + 121), Changes: &changes, ModelCalls: &calls})
		j.ExecutionEffects = append(j.ExecutionEffects, store.FlowEffect{Parent: exec, Kind: "pod", CreatedAt: at(o + 5), CompletedAt: ptr(o + 5.1)})
		// The refusal: verification's last effect is acknowledged long before
		// the verdict, and a refusal has no successor to bound it later.
		j.Verifications = append(j.Verifications, store.FlowVerdict{ID: ver, FlowLink: link, State: "failed", Reason: "comparison-failed", Cleanup: "complete",
			CreatedAt: at(o + 131), ExpiresAt: at(o + 3600), Candidate: exec})
		j.VerificationEffects = append(j.VerificationEffects, store.FlowEffect{Parent: ver, Kind: "tests", CreatedAt: at(o + 135), CompletedAt: ptr(o + 135.6)})
	}
	intake(0, 1)
	admission(0.7, "accepted")
	attempt(0, "b3df41f5-0c1d-4e2f-9a3b-4c5d6e7f8a9b")
	intake(198, 2) // the runner removed the label after the first refusal
	admission(199, "rejected")
	intake(231, 3) // and added it again for the second attempt
	admission(231.4, "accepted")
	attempt(232, "e4af322b-1d2e-4f3a-8b4c-5d6e7f8a9b0c")
	intake(428, 4) // the attempt budget is spent: the label is removed
	admission(429, "rejected")
	return j
}

// Both refusals of the work happened in verification. The rejections of the
// unlabelled revisions that followed are labelled after-work-stopped, so
// nothing reads them as the stage that ended the work. Regenerate the
// fixture with -update-fixtures.
func TestRefusedThenWithdrawnFixture(t *testing.T) {
	events := Derive(refusedThenWithdrawnJournal(), DeriveOptions{})
	var buf bytes.Buffer
	buf.WriteString("# ai-sdlc.flow-event/v1 events derived by TestRefusedThenWithdrawnFixture: two attempts refused in verification, each followed by a rejection of the issue's unlabelled revision\n")
	steps := map[string]int{}
	var refusals []flowevent.Event
	for _, e := range events {
		if err := e.Validate(); err != nil {
			t.Fatal(err)
		}
		if e.Stage == "admission" {
			steps[e.Step+"/"+string(e.Kind)+"/"+e.Diagnostic]++
		}
		if e.Stage == "verification" && e.Step == "verdict" {
			refusals = append(refusals, e)
		}
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(append(line, '\n'))
	}
	want := map[string]int{"decision/completed/": 2, "after-work-stopped/failed/rejected": 2}
	if len(steps) != len(want) {
		t.Fatal("admission steps", steps)
	}
	for k, n := range want {
		if steps[k] != n {
			t.Fatal("admission steps", steps)
		}
	}
	if len(refusals) != 2 {
		t.Fatal("verification verdicts", refusals)
	}
	for _, e := range refusals {
		if e.Kind != flowevent.KindFailed || e.Diagnostic != "comparison-failed" || e.Attributes["reason"] != "comparison-failed" {
			t.Fatalf("verification refusal %+v", e)
		}
	}
	if *updateFixtures {
		if err := os.WriteFile(withdrawnFixture, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	committed, err := os.ReadFile(withdrawnFixture)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, buf.Bytes()) {
		t.Fatal("fixture out of date; run go test ./internal/flowconsole -run TestRefusedThenWithdrawnFixture -update-fixtures")
	}
	if _, err := flowevent.Decode(bytes.NewReader(committed)); err != nil {
		t.Fatal(err)
	}
}

// A rejection stays a plain refusal when it could have ended work: while
// work is in flight, after a newer accepted decision than the stop, before
// any work, or when the latest work completed.
func TestRejectionStaysARefusalUnlessWorkHadStopped(t *testing.T) {
	base := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
	ptr := func(m int) *time.Time { v := at(m); return &v }
	adm := func(issue, m int, decision string) store.FlowAdmission {
		return store.FlowAdmission{Repository: "example/app", Issue: issue, Decision: decision, Risk: "low", CreatedAt: at(m), UpdatedAt: at(m)}
	}
	item := func(n int) string { return "0000000" + string(rune('0'+n)) + "-0c1d-4e2f-9a3b-4c5d6e7f8a9b" }
	work := func(issue, n int) (store.FlowWorkItem, store.FlowLink) {
		return store.FlowWorkItem{UUID: item(n), Repository: "example/app", Issues: []int{issue}, Status: "ready", CreatedAt: at(1), UpdatedAt: at(1)},
			store.FlowLink{WorkItem: item(n), Attempt: 1, Repository: "example/app"}
	}
	w1, l1 := work(1, 1) // issue 1: execution still claimed when rejected
	w2, l2 := work(2, 2) // issue 2: refused, re-accepted, rejected before new work
	w3, l3 := work(3, 3) // issue 3: execution stopped safely, then rejected
	w4, l4 := work(4, 4) // issue 4: verification passed, review queued, rejected
	j := store.FlowJournal{
		ObservedAt: at(60),
		WorkItems:  []store.FlowWorkItem{w1, w2, w3, w4},
		Admissions: []store.FlowAdmission{
			adm(1, 0, "accepted"), adm(1, 10, "rejected"),
			adm(2, 0, "accepted"), adm(2, 12, "accepted"), adm(2, 13, "rejected"),
			adm(5, 10, "rejected"), // issue 5: no work at all
			adm(3, 0, "accepted"), adm(3, 10, "rejected"),
			adm(4, 0, "accepted"), adm(4, 10, "rejected"),
		},
		Executions: []store.FlowExecution{
			{ID: "execution-1", FlowLink: l1, State: "claimed", CreatedAt: at(2), UpdatedAt: at(3), ExpiresAt: at(90), ClaimUntil: ptr(90), LifecycleAt: ptr(3)},
			{ID: "execution-3", FlowLink: l3, State: "stopped", StopReason: "expired", CreatedAt: at(2), UpdatedAt: at(8), ExpiresAt: at(90), LifecycleAt: ptr(3)},
		},
		Verifications: []store.FlowVerdict{
			{ID: "verification-2", FlowLink: l2, State: "failed", Reason: "comparison-failed", Cleanup: "complete", CreatedAt: at(5), ExpiresAt: at(90), Candidate: "execution-2"},
			{ID: "verification-4", FlowLink: l4, State: "passed", Cleanup: "complete", CreatedAt: at(5), ExpiresAt: at(90), Candidate: "execution-4"},
		},
		Reviews: []store.FlowVerdict{{ID: "review-4", FlowLink: l4, State: "queued", Cleanup: "pending", CreatedAt: at(8), ExpiresAt: at(90), Candidate: "execution-4"}},
	}
	steps := map[int]string{}
	for _, e := range Derive(j, DeriveOptions{StaleWait: time.Hour}) {
		if e.Stage == "admission" && e.Attributes["decision"] == "rejected" {
			steps[e.Issue] = e.Step
		}
	}
	want := map[int]string{1: "decision", 2: "decision", 3: "after-work-stopped", 4: "decision", 5: "decision"}
	for issue, w := range want {
		if steps[issue] != w {
			t.Errorf("issue %d: %q, want %q", issue, steps[issue], w)
		}
	}
}

// A critical rejection is a finding about the issue (destructive or
// secret-disclosing work), not a withdrawal: it stays a plain refusal even
// right after a refused attempt.
func TestCriticalRejectionAfterARefusalStaysARefusal(t *testing.T) {
	j := refusedThenWithdrawnJournal()
	j.Admissions[len(j.Admissions)-1].Risk = "critical"
	got := map[string]string{}
	for _, e := range Derive(j, DeriveOptions{}) {
		if e.Stage == "admission" && e.Attributes["decision"] == "rejected" {
			got[e.Attributes["risk"]] = e.Step
		}
	}
	if got["low"] != "after-work-stopped" || got["critical"] != "decision" {
		t.Fatal("steps by risk", got)
	}
}

// The first read that sees a rejection decides its label. Read while
// verification is still running, the rejection is a plain refusal; when the
// verification later fails, the re-derived row keeps the same id, so the hub
// neither relabels nor duplicates it. A fresh console reading only the later
// journal labels the same id after-work-stopped.
func TestRejectionLabelIsDecidedByItsFirstRead(t *testing.T) {
	base := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	ptr := func(s int) *time.Time { v := at(s); return &v }
	const repo, issue, item = "example/app", 47, "5a6b7c8d-0c1d-4e2f-9a3b-4c5d6e7f8a9b"
	link := store.FlowLink{WorkItem: item, Attempt: 1, Repository: repo}
	journal := func(state, reason string) store.FlowJournal {
		return store.FlowJournal{
			ObservedAt: at(600),
			WorkItems:  []store.FlowWorkItem{{UUID: item, Repository: repo, Issues: []int{issue}, Status: "ready", CreatedAt: at(1), UpdatedAt: at(1)}},
			Admissions: []store.FlowAdmission{
				{Repository: repo, Issue: issue, Decision: "accepted", Risk: "low", CreatedAt: at(0), UpdatedAt: at(0)},
				// The label is removed while the tests still run.
				{Repository: repo, Issue: issue, Decision: "rejected", Risk: "low", CreatedAt: at(130), UpdatedAt: at(130)},
			},
			Verifications:       []store.FlowVerdict{{ID: "verification-47", FlowLink: link, State: state, Reason: reason, Cleanup: "complete", CreatedAt: at(100), ExpiresAt: at(3600), ClaimUntil: ptr(3000), Candidate: "execution-47"}},
			VerificationEffects: []store.FlowEffect{{Parent: "verification-47", Kind: "tests", CreatedAt: at(101), CompletedAt: ptr(102)}},
		}
	}
	reads := []store.FlowJournal{journal("claimed", ""), journal("failed", "comparison-failed")}
	hub := NewHub(100, "journal")
	collector := Collector{Hub: hub, Window: 24 * time.Hour, Limit: 100, Read: func(context.Context, time.Time, int) (store.FlowJournal, error) {
		j := reads[0]
		reads = reads[1:]
		return j, nil
	}}
	for range 2 {
		if _, err := collector.Poll(context.Background(), at(600)); err != nil {
			t.Fatal(err)
		}
	}
	rejections := func(h *Hub) []flowevent.Event {
		var out []flowevent.Event
		entries, _, _ := h.Since(0, 0)
		for _, entry := range entries {
			if entry.Event.Stage == "admission" && entry.Event.Attributes["decision"] == "rejected" {
				out = append(out, entry.Event)
			}
		}
		return out
	}
	live := rejections(hub)
	if len(live) != 1 || live[0].Step != "decision" {
		t.Fatal("rejection read during in-flight work", live)
	}
	fresh := NewHub(100, "journal")
	fresh.Publish(Derive(journal("failed", "comparison-failed"), DeriveOptions{}))
	later := rejections(fresh)
	if len(later) != 1 || later[0].Step != "after-work-stopped" || later[0].ID != live[0].ID {
		t.Fatal("rejection derived from the later journal", later, live)
	}
}
