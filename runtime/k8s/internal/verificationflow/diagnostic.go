package verificationflow

import "errors"

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
		return controlled.Stage
	}
	return "unknown"
}

func knownStage(stage string) bool {
	switch stage {
	case "configuration", "authority", "request", "claim", "source", "recipe", "config", "baseline", "candidate", "reference", "tests", "results", "comparison", "cleanup", "commit", "expiry", "queue", "rejection":
		return true
	}
	return false
}
