package execution_test

import (
	"context"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergeflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergegate"
)

// A merge gate policy whose manifest is not the one the authorizing policy
// binds stops at "gate-manifest", both when the publication is selected
// (nothing queued) and at the pre-send fence (nothing sent, not-sent).
func TestMergeGateManifestMismatchNamesItsStop(t *testing.T) {
	t.Run("selection", func(t *testing.T) {
		h := newRetryHarness(t)
		h.changing = false
		var digests []string
		h.r.GatePolicy = func(projectSHA256 string) (mergegate.Policy, error) {
			digests = append(digests, projectSHA256)
			return mergegate.Policy{}, mergegate.ErrManifest
		}
		state, e := h.r.PollOnce(context.Background())
		if e == nil || state != "merge-gate-manifest" || mergeflow.DiagnosticStage(e) != "gate-manifest" {
			t.Fatal("selection stop", state, e, mergeflow.DiagnosticStage(e))
		}
		if len(digests) != 1 || digests[0] != h.f.Policy.ProjectSHA256 {
			t.Fatal("gate policy not resolved for the authorizing policy", digests)
		}
		if h.observations != 0 || h.writes != 0 || h.count(t, `SELECT count(*) FROM merge_requests`) != 0 {
			t.Fatal("a refused gate policy observed, queued or sent", h.observations, h.writes)
		}
	})
	t.Run("pre-send", func(t *testing.T) {
		h := newRetryHarness(t)
		h.changing = false
		calls := 0
		h.r.GatePolicy = func(projectSHA256 string) (mergegate.Policy, error) {
			calls++
			if calls > 1 {
				return mergegate.Policy{}, mergegate.ErrManifest
			}
			return mergegate.PilotPolicy(), nil
		}
		h.stopBeforeSend(t, "gate-manifest")
		if calls != 2 || h.count(t, `SELECT count(*) FROM merge_effects`) != 0 {
			t.Fatal("pre-send stop", calls)
		}
	})
}
