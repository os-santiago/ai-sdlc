package flowconsole

import (
	"os"
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
			{ID: "verification-1", FlowLink: link, State: "passed", Cleanup: "complete", CreatedAt: base, ExpiresAt: base.Add(30 * time.Minute), Candidate: "execution-1"},
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

// A merged issue that is reopened and then refused: only the first
// rejection after the merge is the closure; the later one is a refusal.
func TestOnlyTheFirstRejectionAfterAMergeIsIssueClosed(t *testing.T) {
	base := time.Date(2026, 11, 26, 4, 0, 0, 0, time.UTC)
	item := "8f9a2c1e-3b4d-4e5f-8a6b-7c8d9e0f1a2b"
	merged := base.Add(6 * time.Minute)
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
	j := store.FlowJournal{
		ObservedAt: base.Add(time.Hour),
		WorkItems:  []store.FlowWorkItem{{UUID: item, Repository: "example/app", Issues: []int{258}, Status: "ready", CreatedAt: base, UpdatedAt: base}},
		Admissions: []store.FlowAdmission{
			{Repository: "example/app", Issue: 258, Decision: "accepted", Risk: "low", CreatedAt: base, UpdatedAt: base},
			// Listed out of order on purpose: the derivation orders by time.
			{Repository: "example/app", Issue: 258, Decision: "rejected", Risk: "high", CreatedAt: at(30), UpdatedAt: at(30)}, // reopened, refused
			{Repository: "example/app", Issue: 258, Decision: "rejected", Risk: "low", CreatedAt: at(7), UpdatedAt: at(7)},    // closed by the merge
		},
		Merges: []store.FlowVerdict{{ID: "merge-1", FlowLink: store.FlowLink{WorkItem: item, Attempt: 1, Repository: "example/app"}, State: "merged", CreatedAt: at(5), FinishedAt: &merged}},
	}
	var closedAt, refusedAt []time.Time
	for _, e := range Derive(j, DeriveOptions{}) {
		if e.Step == "after-issue-closed" {
			t.Fatal("a reopened issue's refusal labelled as the closure's")
		}
		if e.Stage == "admission" && e.Step == "issue-closed" {
			closedAt = append(closedAt, e.Time)
		}
		if e.Stage == "admission" && e.Kind == flowevent.KindFailed {
			refusedAt = append(refusedAt, e.Time)
		}
	}
	if len(closedAt) != 1 || !closedAt[0].Equal(at(7)) || len(refusedAt) != 1 || !refusedAt[0].Equal(at(30)) {
		t.Fatal("closed", closedAt, "refused", refusedAt)
	}
}

// The closed issue's next revisions, refused within the closure window at
// the same risk, are labelled after-issue-closed; a refusal after the window,
// at another risk, or after another decision stays a plain refusal.
func TestRefusalsAfterIssueClosed(t *testing.T) {
	base := time.Date(2026, 11, 26, 4, 0, 0, 0, time.UTC)
	item := "8f9a2c1e-3b4d-4e5f-8a6b-7c8d9e0f1a2b"
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
	merged := at(6)
	adm := func(m int, decision, risk string) store.FlowAdmission {
		return store.FlowAdmission{Repository: "example/app", Issue: 258, Decision: decision, Risk: risk, CreatedAt: at(m), UpdatedAt: at(m)}
	}
	j := store.FlowJournal{
		ObservedAt: base.Add(2 * time.Hour),
		WorkItems:  []store.FlowWorkItem{{UUID: item, Repository: "example/app", Issues: []int{258}, Status: "ready", CreatedAt: base, UpdatedAt: base}},
		Admissions: []store.FlowAdmission{
			adm(0, "accepted", "low"),
			adm(7, "rejected", "low"),  // issue-closed
			adm(8, "rejected", "low"),  // after-issue-closed
			adm(9, "rejected", "high"), // another risk: a refusal, and it ends the run
			adm(10, "rejected", "low"), // the run ended: a refusal
			adm(40, "rejected", "low"), // reopened later: a refusal
		},
		Merges: []store.FlowVerdict{{ID: "merge-1", FlowLink: store.FlowLink{WorkItem: item, Attempt: 1, Repository: "example/app"}, State: "merged", CreatedAt: at(5), FinishedAt: &merged}},
	}
	steps := map[time.Time]string{}
	for _, e := range Derive(j, DeriveOptions{}) {
		if e.Stage == "admission" {
			steps[e.Time] = e.Step + "/" + string(e.Kind)
		}
	}
	want := map[int]string{0: "decision/completed", 7: "issue-closed/completed", 8: "after-issue-closed/failed", 9: "decision/failed", 10: "decision/failed", 40: "decision/failed"}
	for m, w := range want {
		if steps[at(m)] != w {
			t.Errorf("minute %d: %s, want %s", m, steps[at(m)], w)
		}
	}
}

// A passed verdict's extension to the next stage's creation never passes
// the verdict row's expiry.
func TestVerdictExtensionStopsAtExpiry(t *testing.T) {
	base := time.Date(2026, 11, 26, 4, 11, 37, 0, time.UTC)
	link := store.FlowLink{WorkItem: "8f9a2c1e-3b4d-4e5f-8a6b-7c8d9e0f1a2b", Attempt: 1, Repository: "example/app"}
	done, expiry := base.Add(6*time.Second), base.Add(10*time.Minute)
	j := store.FlowJournal{
		ObservedAt:          base.Add(time.Hour),
		Verifications:       []store.FlowVerdict{{ID: "verification-1", FlowLink: link, State: "passed", Cleanup: "complete", CreatedAt: base, ExpiresAt: expiry, Candidate: "execution-1"}},
		VerificationEffects: []store.FlowEffect{{Parent: "verification-1", Kind: "tests", CreatedAt: base.Add(time.Second), CompletedAt: &done}},
		Reviews:             []store.FlowVerdict{{ID: "review-1", FlowLink: link, State: "queued", Cleanup: "pending", CreatedAt: base.Add(20 * time.Minute), Candidate: "execution-1"}},
	}
	for _, e := range Derive(j, DeriveOptions{}) {
		if e.Stage == "verification" && e.Kind == flowevent.KindCompleted && !e.Time.Equal(expiry) {
			t.Fatal("extension not capped at the expiry", e.Time)
		}
	}
}

// The live pattern (battery 35, #277): verification claimed at 07:49:38.726,
// every effect acknowledged within 0.35 s, a claim lease that ended seconds
// later, and the review created from the passed verdict at 07:50:31.451.
// The verification bar runs from its claim to that verdict, not to the
// effect acknowledgements.
func TestLiveVerificationSpansClaimToVerdict(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339Nano, "2026-11-26T"+s+"Z"); return v }
	link := store.FlowLink{WorkItem: "8f9a2c1e-3b4d-4e5f-8a6b-7c8d9e0f1a2b", Attempt: 1, Repository: "os-santiago/ai-sdlc-sample"}
	ack := func(s string) *time.Time { v := at(s); return &v }
	lease := at("07:49:48.726")
	j := store.FlowJournal{
		ObservedAt: at("07:55:00"),
		Verifications: []store.FlowVerdict{{ID: "verification-277", FlowLink: link, State: "passed", Cleanup: "complete",
			CreatedAt: at("07:49:34.728"), ExpiresAt: at("08:19:34.728"), ClaimUntil: &lease, Candidate: "execution-277"}},
		VerificationEffects: []store.FlowEffect{
			{Parent: "verification-277", Kind: "config", CreatedAt: at("07:49:38.726"), CompletedAt: ack("07:49:38.743")},
			{Parent: "verification-277", Kind: "baseline", CreatedAt: at("07:49:38.750"), CompletedAt: ack("07:49:38.859")},
			{Parent: "verification-277", Kind: "tests", CreatedAt: at("07:49:38.867"), CompletedAt: ack("07:49:38.959")},
			{Parent: "verification-277", Kind: "candidate", CreatedAt: at("07:49:38.968"), CompletedAt: ack("07:49:39.059")},
		},
		Reviews: []store.FlowVerdict{{ID: "review-277", FlowLink: link, State: "passed", Cleanup: "complete", CreatedAt: at("07:50:31.451"), ExpiresAt: at("08:20:31.451"), Candidate: "execution-277"}},
	}
	var verdict *flowevent.Event
	events := Derive(j, DeriveOptions{})
	for i, e := range events {
		if e.Stage == "verification" && e.Kind == flowevent.KindCompleted {
			verdict = &events[i]
		}
	}
	if verdict == nil || !verdict.Time.Equal(at("07:50:31.451")) || verdict.DurationMs != 52725 {
		t.Fatalf("verification verdict %+v", verdict)
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

// The live battery-35 fixture: each delivered issue ends with an
// issue-closed admission after its merge and, about 34 s later, a refusal of
// the closed issue's next revision; verification spans claim to verdict.
func TestLiveFixturePatterns(t *testing.T) {
	file, err := os.Open("../../components/flow-console/fixtures/live-battery-35.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	events, err := flowevent.Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	type lane struct{ merged, closed, refused, verdict, reviewQueued, claim time.Time }
	lanes := map[int]*lane{}
	for _, e := range events {
		l := lanes[e.Issue]
		if l == nil {
			l = &lane{}
			lanes[e.Issue] = l
		}
		switch {
		case e.Stage == "merge" && e.Kind == flowevent.KindCompleted:
			l.merged = e.Time
		case e.Stage == "admission" && e.Step == "issue-closed":
			l.closed = e.Time
		case e.Stage == "admission" && e.Kind == flowevent.KindFailed:
			if e.Step != "after-issue-closed" {
				t.Fatalf("issue %d refusal after its closure not labelled: %+v", e.Issue, e)
			}
			l.refused = e.Time
		case e.Stage == "verification" && e.Kind == flowevent.KindStarted:
			l.claim = e.Time
		case e.Stage == "verification" && e.Kind == flowevent.KindCompleted:
			l.verdict = e.Time
		case e.Stage == "review" && e.Kind == flowevent.KindWaiting:
			l.reviewQueued = e.Time
		}
	}
	for _, issue := range []int{276, 277} {
		l := lanes[issue]
		if l == nil || l.merged.IsZero() || !l.closed.After(l.merged) || !l.refused.After(l.closed) || l.refused.Sub(l.closed) > time.Minute {
			t.Fatalf("issue %d post-merge pattern %+v", issue, l)
		}
		if d := l.reviewQueued.Sub(l.verdict); d < 0 || d >= time.Millisecond || l.verdict.Sub(l.claim) < 40*time.Second {
			t.Fatalf("issue %d verification does not span claim to verdict %+v", issue, l)
		}
	}
}

// A failed publication whose uncertain effect reconciliation resolved after
// the settle window shows publication / reconciled / completed or not-sent;
// a live acknowledgement within the claim and an unresolved effect do not.
func TestReconciledPublicationEffectsAreShown(t *testing.T) {
	base := time.Date(2026, 11, 26, 4, 0, 0, 0, time.UTC)
	link := store.FlowLink{WorkItem: "8f9a2c1e-3b4d-4e5f-8a6b-7c8d9e0f1a2b", Attempt: 1, Repository: "example/app"}
	until := base.Add(2 * time.Minute)
	live, observed := base.Add(10*time.Second), until.Add(5*time.Minute)
	verdict := func(id string) store.FlowVerdict {
		return store.FlowVerdict{ID: id, FlowLink: link, State: "failed", Reason: "uncertain", Cleanup: "complete", CreatedAt: base, ExpiresAt: base.Add(time.Hour), ClaimUntil: &until, Candidate: "execution-" + id}
	}
	j := store.FlowJournal{
		ObservedAt:   base.Add(time.Hour),
		Publications: []store.FlowVerdict{verdict("p1"), verdict("p2"), verdict("p3")},
		PublicationEffects: []store.FlowEffect{
			{Parent: "p1", Kind: "commit", CreatedAt: base.Add(time.Second), CompletedAt: &live},
			{Parent: "p1", Kind: "branch", CreatedAt: base.Add(2 * time.Second), NotSentAt: &observed},
			{Parent: "p2", Kind: "tree", CreatedAt: base.Add(time.Second), CompletedAt: &observed},
			{Parent: "p3", Kind: "tree", CreatedAt: base.Add(time.Second)},
		},
	}
	got := map[string]string{}
	for _, e := range Derive(j, DeriveOptions{}) {
		if e.Stage == "publication" && e.Step == "reconciled" {
			if e.Kind != flowevent.KindProgressed || !e.Time.Equal(observed) || e.Validate() != nil {
				t.Fatal("reconciled event", e)
			}
			got[e.Attributes["effect"]+"/"+e.Diagnostic] = e.Attributes["reason"]
		}
	}
	if len(got) != 2 || got["branch/not-sent"] != "uncertain" || got["tree/completed"] != "uncertain" {
		t.Fatal("reconciled events", got)
	}
}

// A failed merge whose uncertain effect the merge reconciler resolved after the
// settle window shows merge / reconciled; an unresolved one stays quarantined.
func TestReconciledMergeEffectsAreShown(t *testing.T) {
	base := time.Date(2026, 11, 26, 4, 0, 0, 0, time.UTC)
	link := store.FlowLink{WorkItem: "8f9a2c1e-3b4d-4e5f-8a6b-7c8d9e0f1a2b", Attempt: 1, Repository: "example/app"}
	until := base.Add(30 * time.Second)
	observed := until.Add(3 * time.Minute)
	verdict := func(id string) store.FlowVerdict {
		return store.FlowVerdict{ID: id, FlowLink: link, State: "failed", Reason: "uncertain", Cleanup: "complete", CreatedAt: base, ExpiresAt: until, ClaimUntil: &until, Candidate: "execution-" + id}
	}
	j := store.FlowJournal{
		ObservedAt: base.Add(time.Hour),
		Merges:     []store.FlowVerdict{verdict("m1"), verdict("m2")},
		MergeEffects: []store.FlowEffect{
			{Parent: "m1", Kind: "merge", CreatedAt: base.Add(time.Second), CompletedAt: &observed},
			{Parent: "m2", Kind: "merge", CreatedAt: base.Add(time.Second)},
		},
	}
	reconciled, quarantined := 0, 0
	for _, e := range Derive(j, DeriveOptions{}) {
		if e.Stage != "merge" {
			continue
		}
		if e.Step == "reconciled" {
			reconciled++
			if e.Kind != flowevent.KindProgressed || e.Diagnostic != "completed" || e.Attributes["effect"] != "merge" || !e.Time.Equal(observed) || e.Validate() != nil {
				t.Fatal("reconciled merge event", e)
			}
		}
		if e.Kind == flowevent.KindQuarantined {
			quarantined++
		}
	}
	if reconciled != 1 || quarantined != 1 {
		t.Fatal("merge reconciliation events", reconciled, quarantined)
	}
}
