package verificationflow

import (
	"errors"
	"strings"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
)

// cleanupAfter is a failed cleanup that followed a stop at stage. It unwraps
// only to the cleanup failure, so a refusal it follows still counts as
// uncertain, as before.
type cleanupAfter struct {
	cause error
	stage string
}

func (e cleanupAfter) Error() string { return e.cause.Error() }
func (e cleanupAfter) Unwrap() error { return e.cause }

// DiagnosticStage is bounded controller metadata, never API or candidate output.
func DiagnosticStage(err error) string {
	var after cleanupAfter
	if errors.As(err, &after) && knownStage(after.stage) && after.stage != "cleanup" {
		return "cleanup-after-" + after.stage
	}
	var controlled ControllerError
	if errors.As(err, &controlled) && knownStage(controlled.Stage) {
		if reason := podtransport.RuntimeClassStopReason(err); controlled.Stage == "sandbox-runtime-class" && reason != "" {
			return controlled.Stage + "-" + reason
		}
		return controlled.Stage
	}
	// A refusal is a correct verdict on the candidate, not a controller
	// fault: name it, with its reason only when that is a known constant.
	var refused DecisionError
	if errors.As(err, &refused) {
		if refusalReason(refused.Reason) {
			return "refused-" + refused.Reason
		}
		return "refused"
	}
	return "unknown"
}

// refusalReason bounds the repository test decision reasons the log may name.
func refusalReason(reason string) bool {
	switch reason {
	case "incomplete-evidence", "invalid-delta", "harness-changed", "test-deleted", "no-test-change", "uncertain",
		"candidate-failed", "no-cross-report", "regression", "no-failing-test", "baseline-failing":
		return true
	}
	return false
}

// createStop names which step of creating a verification resource failed.
// Every value still stops the run the same way; it only refines the log.
type createStop struct{ step string }

func (e createStop) Error() string { return verification.ErrVerification.Error() }

func createStage(stage string, err error) string {
	var stop createStop
	if errors.As(err, &stop) {
		return stage + "-" + stop.step
	}
	return stage
}

// testsStage refines a repository-tests run stage with the bounded
// observation or import step that failed, for example
// candidate-import-transfer-timeout.
func testsStage(stage string, err error) string {
	if step := podtransport.TestsStep(err); step != "" {
		return stage + "-" + step
	}
	return stage
}

func knownStage(stage string) bool {
	switch stage {
	case "source-rate-limited", "sandbox-runtime-class", "runs-deadline", "runs-renew", "runs-context":
		return true
	}
	if base, step, ok := strings.Cut(stage, "-"); ok {
		switch base {
		case "config", "baseline", "candidate", "reference", "tests":
			switch step {
			case "fence", "ledger", "create", "acknowledge":
				return true
			}
		}
		switch base {
		case "baseline", "candidate", "tests":
			// Repository-tests run steps.
			switch step {
			case "import-renew", "import-fence", "unimported-success":
				return true
			}
			return podtransport.KnownTestsStep(step)
		}
		return false
	}
	switch stage {
	case "configuration", "authority", "request", "claim", "source", "recipe", "config", "baseline", "candidate", "reference", "tests", "results", "comparison", "cleanup", "commit", "expiry", "queue", "rejection":
		return true
	}
	return false
}
