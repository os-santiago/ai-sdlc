package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/reviewflow"
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
