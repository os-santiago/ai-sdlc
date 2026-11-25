package metrics_test

import (
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/metrics"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/reviewflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
)

// The bounded diagnostic stages, including the longest composed ones, are
// valid label values: a stop is counted under the stage its log line names,
// not folded into "other".
func TestDiagnosticStagesAreValidLabelValues(t *testing.T) {
	stage := metrics.Bounded("stage")
	stages := []string{
		execution.DiagnosticStage(execution.ControllerError{Stage: "model-publication-worker-exit-137"}),
		execution.DiagnosticStage(execution.ControllerError{Stage: "model-authorization-publication-claim-expired"}),
		execution.DiagnosticStage(execution.ControllerError{Stage: "source-preflight-authority"}),
		execution.DiagnosticStage(execution.ControllerError{Stage: "unlisted"}),
		execution.DiagnosticStage(nil),
		reviewflow.DiagnosticStage(reviewflow.ControllerError{Stage: "model-worker-exit-124"}),
		reviewflow.DiagnosticStage(reviewflow.ControllerError{Stage: "preparation-source-rate-limited"}),
		reviewflow.DiagnosticStage(reviewflow.ErrReview),
		verificationflow.DiagnosticStage(verificationflow.DecisionError{Reason: "baseline-failing"}),
		verificationflow.DiagnosticStage(verificationflow.ControllerError{Stage: "candidate-import-fence"}),
		modelbroker.SafeFailureStage("publication-helper-exit-255"),
		modelbroker.SafeFailureStage("authorization-reservation-model-policy"),
	}
	want := []string{"model-publication-worker-exit-137", "model-authorization-publication-claim-expired", "source-preflight-authority", "unknown", "authority-or-storage",
		"model-worker-exit-124", "preparation-source-rate-limited", "rejected", "refused-baseline-failing", "candidate-import-fence",
		"publication-helper-exit-255", "authorization-reservation-model-policy"}
	for i, s := range stages {
		if s != want[i] {
			t.Fatalf("diagnostic %d = %q, want %q", i, s, want[i])
		}
		if stage.Value(s) != s {
			t.Fatalf("stage %q folded into %q", s, stage.Value(s))
		}
	}
}
