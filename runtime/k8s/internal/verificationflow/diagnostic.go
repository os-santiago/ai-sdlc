package verificationflow

import (
	"errors"
	"strings"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/secretscan"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/sourcefetch"
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

// sourceStage names a source failure. Each stops the run like any other
// source failure; the name tells an operator what to do: wait for the forge's
// rate limit, or repair the installation's read credential (rejected by the
// forge, or missing, malformed or bound to another repository). A credential
// failure never falls back to an anonymous read.
func sourceStage(err error) string {
	switch {
	case sourcefetch.RateLimited(err):
		return "source-rate-limited"
	case sourcefetch.CredentialRejected(err):
		return "source-credential-rejected"
	case sourcefetch.CredentialInvalid(err):
		return "source-credential-invalid"
	}
	return "source"
}

// refusalReason bounds the decision reasons the log may name: the repository
// test rules and the candidate baseline.
func refusalReason(reason string) bool {
	switch reason {
	case "incomplete-evidence", "invalid-delta", "harness-changed", "test-deleted", "no-test-change", "uncertain",
		"candidate-failed", "no-cross-report", "regression", "no-failing-test", "baseline-failing",
		SecretRefusal, "candidate-analysis":
		return true
	}
	return false
}

// RefusalDetail is the bounded detail of a secret refusal for the stop log,
// for example "rule=github-token class=code", or "" for any other error. It
// never carries the matched text or the path.
func RefusalDetail(err error) string {
	var refused DecisionError
	if !errors.As(err, &refused) || refused.Reason != SecretRefusal ||
		!secretscan.KnownRule(refused.Secret.Rule) || !secretscan.KnownClass(refused.Secret.PathClass) {
		return ""
	}
	return "rule=" + refused.Secret.Rule + " class=" + refused.Secret.PathClass
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
	case "source-rate-limited", "source-credential-rejected", "source-credential-invalid", "sandbox-runtime-class", "runs-deadline", "runs-renew", "runs-context",
		string(stopDependencyKey), string(stopDependencyUnavailable):
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
