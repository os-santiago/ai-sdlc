package verificationflow

import "errors"

// DiagnosticStage is bounded controller metadata, never API or candidate output.
func DiagnosticStage(err error) string {
	var controlled ControllerError
	if errors.As(err, &controlled) {
		switch controlled.Stage {
		case "configuration", "authority", "request", "claim", "source", "recipe", "config", "baseline", "candidate", "reference", "tests", "results", "comparison", "cleanup", "commit", "expiry", "queue", "rejection":
			return controlled.Stage
		}
	}
	return "unknown"
}
