package reviewflow

import (
	"errors"
	"fmt"
	"testing"
)

func TestReviewDiagnosticStageIsBounded(t *testing.T) {
	for err, want := range map[error]string{
		ControllerError{Stage: "readiness"}:                      "readiness",
		fmt.Errorf("wrapped: %w", ControllerError{Stage: "pod"}): "pod",
		ControllerError{Stage: "sensitive-fixture"}:              "unknown",
		errors.New("sensitive-fixture"):                          "unknown",
		ErrReview:                                                "rejected",
		ControllerError{Stage: "model-evidence"}:                 "model-evidence",
		ControllerError{Stage: "model-worker-exit-23"}:           "model-worker-exit-23",
		ControllerError{Stage: "model-worker-exit-256"}:          "model-unknown",
		ControllerError{Stage: "model-provider"}:                 "model-provider",
	} {
		if got := DiagnosticStage(err); got != want {
			t.Fatalf("%v: got %q, want %q", err, got, want)
		}
	}
	if got := DiagnosticStage(ControllerError{Stage: "model-sensitive-fixture"}); got == "model-sensitive-fixture" {
		t.Fatal("unbounded model stage")
	}
}
