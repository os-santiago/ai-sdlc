package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergeflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergegate"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/metrics"
)

// Only a claimed publication or merge attempt is a delivery; idle and
// standing quarantine states, polled every cycle, are not counted.
func TestDeliveriesCountOnlyAttemptOutcomes(t *testing.T) {
	for state, outcome := range map[string]string{
		"pull-request-published": "published", "publication-stopped": "publication-stopped",
		"merge-complete": "merged", "merge-stopped": "merge-stopped",
	} {
		before, _ := metrics.Default.Sample("ai_sdlc_deliveries_total", outcome)
		countDelivery(state)
		if after, _ := metrics.Default.Sample("ai_sdlc_deliveries_total", outcome); after != before+1 {
			t.Fatal(state)
		}
	}
	for _, state := range []string{"", "publication-idle", "publication-quarantined", "publication-reconciled", "merge-quarantined", "merge-reconciled", "merge-waiting-for-checks", "merge-waiting-for-review-threads", "merge-authority-denied", "merge-pull-request-closed", "merge-pull-request-merged-outside"} {
		if deliveryOutcome(state) != "" {
			t.Fatal("standing state counted", state)
		}
	}
	for _, state := range mergeflow.WaitStates() {
		if deliveryOutcome(state) != "" {
			t.Fatal("wait counted as a delivery", state)
		}
	}
	for _, outcome := range metrics.DeliveryOutcomes {
		found := false
		for _, state := range []string{"pull-request-published", "publication-stopped", "merge-complete", "merge-stopped"} {
			found = found || deliveryOutcome(state) == outcome
		}
		if !found {
			t.Fatal("documented outcome never produced", outcome)
		}
	}
}

// A stopped merge run names its bounded stage; nothing else is echoed.
func TestMergeStopLineNamesTheStage(t *testing.T) {
	for _, c := range []struct {
		state string
		err   error
		want  string
	}{
		{"merge-stopped", mergeflow.StopError{Stage: "gate-changed-checks"}, "merge reconciliation stopped safely: merge-stopped stage=gate-changed-checks"},
		{"merge-stopped", errors.New("forge body"), "merge reconciliation stopped safely: merge-stopped stage=unknown"},
		{"merge-stopped", mergeflow.StopError{Stage: "gate-unavailable", Exhausted: true}, "merge reconciliation stopped safely: merge-stopped stage=gate-unavailable attempts=exhausted"},
		{"merge-quarantined", mergeflow.ErrMerge, "merge reconciliation stopped safely: merge-quarantined"},
	} {
		if got := mergeStopLine(c.state, c.err); got != c.want {
			t.Errorf("mergeStopLine(%q) = %q, want %q", c.state, got, c.want)
		}
	}
}

// A wait names the pull request, every blocker and the thread count; nothing
// else is echoed, and a state that is not the reported wait adds nothing.
func TestMergeStateLineNamesTheBlockers(t *testing.T) {
	threads := &mergeflow.Wait{State: "merge-waiting-for-review-threads", PullRequest: 11, Reasons: []string{"review-threads-unresolved", "native-merge-not-ready"}, UnresolvedThreads: 1}
	for _, c := range []struct {
		state string
		wait  *mergeflow.Wait
		want  string
	}{
		{"merge-waiting-for-review-threads", threads, "merge reconciliation: merge-waiting-for-review-threads pull_request=11 reasons=review-threads-unresolved,native-merge-not-ready unresolved_threads=1"},
		{"merge-waiting-for-checks", &mergeflow.Wait{State: "merge-waiting-for-checks", PullRequest: 7, Reasons: []string{"checks-not-passed"}}, "merge reconciliation: merge-waiting-for-checks pull_request=7 reasons=checks-not-passed"},
		{"merge-reconciled", nil, "merge reconciliation: merge-reconciled"},
		{"merge-complete", threads, "merge reconciliation: merge-complete"},
	} {
		if got := mergeStateLine(c.state, c.wait); got != c.want {
			t.Errorf("mergeStateLine(%q) = %q, want %q", c.state, got, c.want)
		}
	}
	if strings.Join(metrics.MergeWaitReasons, ",") != strings.Join(mergegate.Reasons, ",") {
		t.Fatal("metric reasons differ from the gate's", metrics.MergeWaitReasons)
	}
	unknown := &mergeflow.Wait{State: "merge-waiting-for-review-threads", PullRequest: 12, Reasons: []string{"review-threads-unresolved"}, UnresolvedThreadsUnknown: true}
	if got := mergeStateLine(unknown.State, unknown); got != "merge reconciliation: merge-waiting-for-review-threads pull_request=12 reasons=review-threads-unresolved unresolved_threads=unknown" {
		t.Fatal("unknown thread count", got)
	}
	sampleMergeWait(mergeflow.WaitSummary{Reasons: map[string]int{"review-threads-unresolved": 2, "native-merge-not-ready": 1}, UnresolvedThreads: 3})
	if v, _ := metrics.Default.Sample("ai_sdlc_merge_waiting", "review-threads-unresolved"); v != 2 {
		t.Fatal("thread blocker not sampled", v)
	}
	if v, _ := metrics.Default.Sample("ai_sdlc_merge_waiting", "checks-not-passed"); v != 0 {
		t.Fatal("absent blocker sampled", v)
	}
	if v, _ := metrics.Default.Sample("ai_sdlc_merge_unresolved_review_threads"); v != 3 {
		t.Fatal("thread count not sampled", v)
	}
	sampleMergeWait(mergeflow.WaitSummary{Reasons: map[string]int{}})
	if v, _ := metrics.Default.Sample("ai_sdlc_merge_waiting", "review-threads-unresolved"); v != 0 {
		t.Fatal("a finished wait kept its blocker", v)
	}
}
