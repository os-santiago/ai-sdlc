package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/metrics"
)

// Each stopped candidate cycle is counted under the same bounded stage its
// log line names; an arbitrary error is counted only as its fixed category.
func TestCandidateStopCountsTheLoggedStage(t *testing.T) {
	for _, tc := range []struct {
		err   error
		stage string
	}{
		{execution.ControllerError{Stage: "model-provider"}, "model-provider"},
		{execution.ControllerError{Stage: "export"}, "export"},
		{execution.ControllerError{Stage: "free text from os-santiago/private"}, "unknown"},
		{errors.New("pq: password authentication failed for user x"), "authority-or-storage"},
	} {
		before, _ := metrics.Default.Sample("ai_sdlc_stops_total", "worker", "candidate", tc.stage)
		line := candidateStopped(tc.err)
		if !strings.HasPrefix(line, "candidate reconciliation stopped safely: "+tc.stage+" ") {
			t.Fatal(line)
		}
		if after, _ := metrics.Default.Sample("ai_sdlc_stops_total", "worker", "candidate", tc.stage); after != before+1 {
			t.Fatal(tc.stage, before, after)
		}
	}
}
