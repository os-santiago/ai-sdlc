package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/metrics"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/reviewflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
)

func TestPublicationFailureNeverPrecedesCleanupOrStarvesNextCycle(t *testing.T) {
	for _, reviewState := range []string{"", "rejected", "review-passed"} {
		t.Run(reviewState, func(t *testing.T) {
			var calls []string
			review := func(context.Context) (string, error) {
				calls = append(calls, "review-cleanup-and-poll")
				return reviewState, nil
			}
			verify := func(context.Context) (string, error) {
				calls = append(calls, "verification-cleanup-and-poll")
				return "behavior-verified", nil
			}
			handoff := func(context.Context) (bool, error) {
				calls = append(calls, "handoff")
				return false, errors.New("stale proof")
			}
			reconcileQueues(context.Background(), review, verify, handoff)
			reconcileQueues(context.Background(), review, verify, handoff)
			want := []string{"review-cleanup-and-poll", "verification-cleanup-and-poll", "handoff", "review-cleanup-and-poll", "verification-cleanup-and-poll", "handoff"}
			if !reflect.DeepEqual(calls, want) {
				t.Fatal(calls)
			}
		})
	}
}

func TestUncertainCleanupStopsHandoff(t *testing.T) {
	for _, stage := range []string{"review", "verification"} {
		calls := 0
		review := func(context.Context) (string, error) {
			if stage == "review" {
				return "cleanup-uncertain", reviewflow.ControllerError{Stage: "cleanup", CleanupPending: true}
			}
			return "review-passed", nil
		}
		verify := func(context.Context) (string, error) { return "cleanup-pending", errors.New("cleanup pending") }
		reconcileQueues(context.Background(), review, verify, func(context.Context) (bool, error) { calls++; return true, nil })
		if calls != 0 {
			t.Fatal("handoff bypassed uncertain cleanup")
		}
	}
}

// A missing sandbox RuntimeClass stops only new claims: each poll has already
// finished its cleanup, so verification still runs and approved work is still
// handed off in the same cycle.
func TestMissingSandboxClassStillHandsOffApprovedWork(t *testing.T) {
	reviewSandbox := func(context.Context) (string, error) {
		return "sandbox-runtime-class-unavailable", reviewflow.ControllerError{Stage: "sandbox-runtime-class"}
	}
	verifySandbox := func(context.Context) (string, error) {
		return "sandbox-runtime-class-unavailable", verificationflow.ControllerError{Stage: "sandbox-runtime-class"}
	}
	verifyIdle := func(context.Context) (string, error) { return "", nil }
	reviewIdle := func(context.Context) (string, error) { return "", nil }
	for label, polls := range map[string][2]queuePoll{
		"review":       {reviewSandbox, verifyIdle},
		"verification": {reviewIdle, verifySandbox},
		"both":         {reviewSandbox, verifySandbox},
	} {
		calls := 0
		reconcileQueues(context.Background(), polls[0], polls[1], func(context.Context) (bool, error) { calls++; return true, nil })
		if calls != 1 {
			t.Fatal(label, "handoff blocked by the sandbox class check")
		}
	}
	// Other verification failures still stop the handoff.
	calls := 0
	reconcileQueues(context.Background(), reviewIdle, func(context.Context) (string, error) {
		return "stopped", verificationflow.ControllerError{Stage: "pod"}
	}, func(context.Context) (bool, error) { calls++; return true, nil })
	if calls != 0 {
		t.Fatal("handoff ran after a post-claim verification failure")
	}
}

// Every "stopped safely" line is counted under its loop and the same bounded
// stage the line names.
func TestStoppedLoopsAreCountedUnderTheLoggedStage(t *testing.T) {
	count := func(loop, stage string) float64 {
		v, _ := metrics.Default.Sample("ai_sdlc_stops_total", "verifier", loop, stage)
		return v
	}
	review, verification, handoff := count("review", "model-provider"), count("verification", "refused-regression"), count("publication-handoff", metrics.Unclassified)
	reconcileQueues(context.Background(),
		func(context.Context) (string, error) { return "", reviewflow.ControllerError{Stage: "model-provider"} },
		func(context.Context) (string, error) { return "", nil }, nil)
	reconcileQueues(context.Background(), nil,
		func(context.Context) (string, error) {
			return "", verificationflow.DecisionError{Reason: "regression"}
		}, nil)
	reconcileQueues(context.Background(), nil,
		func(context.Context) (string, error) { return "", nil },
		func(context.Context) (bool, error) { return false, errors.New("stale proof") })
	if count("review", "model-provider") != review+1 || count("verification", "refused-regression") != verification+1 || count("publication-handoff", metrics.Unclassified) != handoff+1 {
		t.Fatal("stop not counted under its loop and stage")
	}
}
