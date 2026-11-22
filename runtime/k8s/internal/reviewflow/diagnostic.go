package reviewflow

import (
	"errors"
	"strings"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
)

// DiagnosticStage is bounded controller metadata for the stop log, never
// model, repository or API output.
func DiagnosticStage(err error) string {
	var stopped ControllerError
	if errors.As(err, &stopped) {
		if strings.HasPrefix(stopped.Stage, "model-") {
			return "model-" + modelbroker.SafeFailureStage(strings.TrimPrefix(stopped.Stage, "model-"))
		}
		switch stopped.Stage {
		case "authority", "binding", "claim", "cleanup", "commit", "config", "configuration", "expiry", "export",
			"gateway-profile-unfit", "gateway-unavailable", "import", "mailbox-read", "model", "observation", "pod",
			"preparation", "queue", "readiness", "rejection", "rejection-commit", "request-changes", "start", "template":
			return stopped.Stage
		}
	}
	if errors.Is(err, ErrReview) {
		return "rejected"
	}
	return "unknown"
}
