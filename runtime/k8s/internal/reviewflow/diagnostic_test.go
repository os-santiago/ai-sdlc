package reviewflow

import (
	"errors"
	"fmt"
	"testing"
)

func TestReviewDiagnosticStageIsBounded(t *testing.T) {
	for err, want := range map[error]string{
		ControllerError{Stage: "readiness"}:                         "readiness",
		fmt.Errorf("wrapped: %w", ControllerError{Stage: "pod"}):    "pod",
		ControllerError{Stage: "sensitive-fixture"}:                 "unknown",
		errors.New("sensitive-fixture"):                             "unknown",
		ErrReview:                                                   "rejected",
		ControllerError{Stage: "model-evidence"}:                    "model-evidence",
		ControllerError{Stage: "model-worker-exit-23"}:              "model-worker-exit-23",
		ControllerError{Stage: "model-worker-exit-256"}:             "model-unknown",
		ControllerError{Stage: "model-worker-exit-0"}:               "model-worker-exit-0",
		ControllerError{Stage: "model-worker-exit-00"}:              "model-unknown",
		ControllerError{Stage: "model-provider"}:                    "model-provider",
		ControllerError{Stage: "cleanup-pod"}:                       "cleanup-pod",
		ControllerError{Stage: "cleanup-pod-observe-timeout"}:       "cleanup-pod-observe-timeout",
		ControllerError{Stage: "cleanup-config-observe-status-5xx"}: "cleanup-config-observe-status-5xx",
		ControllerError{Stage: "model-mailbox-read-helper-exit-2"}:  "model-mailbox-read-helper-exit-2",
		ControllerError{Stage: "model-mailbox-read-sensitive"}:      "model-unknown",
		ControllerError{Stage: "cleanup-pod-sensitive"}:             "unknown",
		ControllerError{Stage: "preparation-source-rate-limited"}:   "preparation-source-rate-limited",
		ControllerError{Stage: "preparation-sensitive"}:             "unknown",
	} {
		if got := DiagnosticStage(err); got != want {
			t.Fatalf("%v: got %q, want %q", err, got, want)
		}
	}
	if got := DiagnosticStage(ControllerError{Stage: "model-sensitive-fixture"}); got == "model-sensitive-fixture" {
		t.Fatal("unbounded model stage")
	}
}

func TestSourceRateLimitStillStopsAsARejectedPreparation(t *testing.T) {
	if !errors.Is(errSourceRateLimited, ErrReview) {
		t.Fatal("a rate-limited preparation must stop like any other")
	}
}
