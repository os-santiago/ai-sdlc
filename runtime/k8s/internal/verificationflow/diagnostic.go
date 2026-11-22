package verificationflow

import (
	"errors"
	"strings"

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
		return controlled.Stage
	}
	return "unknown"
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

func knownStage(stage string) bool {
	if base, step, ok := strings.Cut(stage, "-"); ok {
		switch base {
		case "config", "baseline", "candidate", "reference", "tests":
			switch step {
			case "fence", "ledger", "create", "acknowledge":
				return true
			}
		case "source":
			switch step {
			case "rate-limited", "deadline":
				return true
			}
		}
		return false
	}
	switch stage {
	case "configuration", "authority", "request", "claim", "source", "recipe", "config", "baseline", "candidate", "reference", "tests", "results", "comparison", "cleanup", "commit", "expiry", "queue", "rejection":
		return true
	}
	return false
}
