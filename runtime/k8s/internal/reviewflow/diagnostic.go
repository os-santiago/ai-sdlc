package reviewflow

import (
	"errors"
	"regexp"
	"strings"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
)

// DiagnosticStage is bounded controller metadata for the stop log, never
// model, repository or API output.
func DiagnosticStage(err error) string {
	var stopped ControllerError
	if errors.As(err, &stopped) {
		if strings.HasPrefix(stopped.Stage, "model-") {
			detail := strings.TrimPrefix(stopped.Stage, "model-")
			switch {
			case reviewModelStage(detail):
				return stopped.Stage
			case workerExit.MatchString(detail):
				return stopped.Stage
			}
			return "model-" + modelbroker.SafeFailureStage(detail)
		}
		switch stopped.Stage {
		case "authority", "binding", "claim", "cleanup", "cleanup-pod", "cleanup-config", "cleanup-confirm", "commit", "config", "configuration", "expiry", "export",
			"gateway-profile-unfit", "gateway-unavailable", "import", "model", "observation", "pod",
			"preparation", "preparation-source-rate-limited", "queue", "readiness", "rejection", "rejection-commit", "request-changes", "start", "template":
			return stopped.Stage
		}
		for _, step := range []string{"cleanup-pod-", "cleanup-config-"} {
			if detail, ok := strings.CutPrefix(stopped.Stage, step); ok && podtransport.KnownRemovalStep(detail) {
				return stopped.Stage
			}
		}
	}
	if errors.Is(err, ErrReview) {
		return "rejected"
	}
	return "unknown"
}

// The review controller's own model stages, beyond the shared dispatch ones.
var workerExit = regexp.MustCompile(`^worker-exit-(0|[1-9][0-9]?|1[0-9][0-9]|2[0-4][0-9]|25[0-5])$`)

func reviewModelStage(stage string) bool {
	switch stage {
	case "evidence", "mailbox-read", "response-policy", "authorization-completion", "authorization-acknowledgement",
		"drive-deadline", "drive-authority", "drive-observe", "drive-exit-race", "drive-sequence":
		return true
	}
	return false
}

// removalStage refines a cleanup step with the bounded removal step that
// failed, for example cleanup-pod-observe-timeout.
func removalStage(step string, err error) string {
	if detail := podtransport.RemovalStep(err); podtransport.KnownRemovalStep(detail) {
		return step + "-" + detail
	}
	return step
}
