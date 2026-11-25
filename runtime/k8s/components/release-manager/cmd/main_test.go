package main

import (
	"testing"

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
	for _, state := range []string{"", "publication-idle", "publication-quarantined", "merge-quarantined", "merge-waiting-for-checks", "merge-authority-denied"} {
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
