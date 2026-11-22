package verificationflow

import (
	"errors"
	"fmt"
	"testing"
)

func TestDiagnosticsNeverIncludeUntrustedErrors(t *testing.T) {
	for _, err := range []error{errors.New("sensitive-fixture"), ControllerError{"sensitive-fixture"}, nil} {
		if DiagnosticStage(err) != "unknown" {
			t.Fatal("untrusted diagnostic")
		}
	}
	if DiagnosticStage(fmt.Errorf("wrapper: %w", ControllerError{"config"})) != "config" {
		t.Fatal("bounded wrapped stage")
	}
}

func TestDiagnosticKeepsTheStageBeforeAFailedCleanup(t *testing.T) {
	err := cleanupAfter{cause: ControllerError{"cleanup"}, stage: "baseline"}
	if DiagnosticStage(err) != "cleanup-after-baseline" {
		t.Fatal(DiagnosticStage(err))
	}
	var refused DecisionError
	if errors.As(cleanupAfter{cause: ControllerError{"cleanup"}, stage: "comparison"}, &refused) {
		t.Fatal("a failed cleanup must not expose the refusal it followed")
	}
	if DiagnosticStage(cleanupAfter{cause: ControllerError{"cleanup"}, stage: "sensitive-fixture"}) != "cleanup" {
		t.Fatal("unbounded stage")
	}
}

func TestDiagnosticNamesTheFailedCreateStep(t *testing.T) {
	for _, step := range []string{"fence", "ledger", "create", "acknowledge"} {
		stage := createStage("config", createStop{step})
		if DiagnosticStage(ControllerError{stage}) != "config-"+step {
			t.Fatal(stage)
		}
	}
	if createStage("baseline", errors.New("other")) != "baseline" {
		t.Fatal("unrefined stage changed")
	}
	if DiagnosticStage(ControllerError{"config-sensitive"}) != "unknown" || DiagnosticStage(ControllerError{"queue-fence"}) != "unknown" {
		t.Fatal("unbounded create step")
	}
}

func TestDiagnosticKeepsTheRefinedStepAfterAFailedCleanup(t *testing.T) {
	stage := createStage("config", createStop{"acknowledge"})
	if got := DiagnosticStage(cleanupAfter{cause: ControllerError{"cleanup"}, stage: stage}); got != "cleanup-after-config-acknowledge" {
		t.Fatal(got)
	}
}

func TestDiagnosticNamesTheSourceRateLimit(t *testing.T) {
	if got := DiagnosticStage(ControllerError{"source-rate-limited"}); got != "source-rate-limited" {
		t.Fatal(got)
	}
	if got := DiagnosticStage(cleanupAfter{cause: ControllerError{"cleanup"}, stage: "source-rate-limited"}); got != "cleanup-after-source-rate-limited" {
		t.Fatal(got)
	}
	if DiagnosticStage(ControllerError{"source-sensitive"}) != "unknown" {
		t.Fatal("unbounded source detail")
	}
}
