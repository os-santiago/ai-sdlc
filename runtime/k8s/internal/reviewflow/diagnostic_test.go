package reviewflow

import (
	"errors"
	"fmt"
	"testing"
)

func TestReviewDiagnosticStageIsBounded(t *testing.T) {
	for err, want := range map[error]string{
		ControllerError{Stage: "readiness"}:                       "readiness",
		ControllerError{Stage: "readiness-deadline"}:              "readiness-deadline",
		fmt.Errorf("wrapped: %w", ControllerError{Stage: "pod"}):  "pod",
		ControllerError{Stage: "sensitive-fixture"}:               "unknown",
		errors.New("sensitive-fixture"):                           "unknown",
		ErrReview:                                                 "rejected",
		ControllerError{Stage: "model-evidence"}:                  "model-evidence",
		ControllerError{Stage: "model-worker-exit-23"}:            "model-worker-exit-23",
		ControllerError{Stage: "model-worker-exit-256"}:           "model-unknown",
		ControllerError{Stage: "model-provider"}:                  "model-provider",
		ControllerError{Stage: "cleanup-pod"}:                     "cleanup-pod",
		ControllerError{Stage: "preparation"}:                     "preparation",
		ControllerError{Stage: "preparation-authority"}:           "preparation-authority",
		ControllerError{Stage: "preparation-proof"}:               "preparation-proof",
		ControllerError{Stage: "preparation-revalidate"}:          "preparation-revalidate",
		ControllerError{Stage: "preparation-source-unavailable"}:  "preparation-source-unavailable",
		ControllerError{Stage: "preparation-source-rate-limited"}: "preparation-source-rate-limited",
		ControllerError{Stage: "preparation-source-deadline"}:     "preparation-source-deadline",
		ControllerError{Stage: "preparation-sensitive-fixture"}:   "unknown",
	} {
		if got := DiagnosticStage(err); got != want {
			t.Fatalf("%v: got %q, want %q", err, got, want)
		}
	}
	if got := DiagnosticStage(ControllerError{Stage: "model-sensitive-fixture"}); got == "model-sensitive-fixture" {
		t.Fatal("unbounded model stage")
	}
}
