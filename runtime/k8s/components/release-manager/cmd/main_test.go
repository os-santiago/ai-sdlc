package main

import (
	"errors"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergeflow"

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
	for _, state := range []string{"", "publication-idle", "publication-quarantined", "publication-reconciled", "merge-quarantined", "merge-reconciled", "merge-waiting-for-checks", "merge-authority-denied", "merge-pull-request-closed", "merge-pull-request-merged-outside"} {
		if deliveryOutcome(state) != "" {
			t.Fatal("standing state counted", state)
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
