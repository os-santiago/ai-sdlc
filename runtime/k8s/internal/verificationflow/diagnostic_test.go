package verificationflow

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
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

func TestDiagnosticNamesARefusalAndOnlyItsKnownReason(t *testing.T) {
	if got := DiagnosticStage(DecisionError{Reason: "candidate-failed"}); got != "refused-candidate-failed" {
		t.Fatal(got)
	}
	if got := DiagnosticStage(fmt.Errorf("wrapped: %w", DecisionError{Reason: "regression"})); got != "refused-regression" {
		t.Fatal(got)
	}
	if got := DiagnosticStage(DecisionError{Reason: "sensitive-fixture"}); got != "refused" {
		t.Fatal("unbounded refusal reason", got)
	}
	// A failed cleanup after a refusal keeps the existing cleanup diagnostic.
	if got := DiagnosticStage(cleanupAfter{cause: ControllerError{"cleanup"}, stage: "comparison"}); got != "cleanup-after-comparison" {
		t.Fatal(got)
	}
}

// Every refusal reason the repository test decision can return is nameable.
func TestEveryDecisionRefusalReasonIsKnown(t *testing.T) {
	raw, err := os.ReadFile("../repositorytests/decide.go")
	if err != nil {
		t.Fatal(err)
	}
	found := regexp.MustCompile(`result\(false, "([a-z-]+)"\)`).FindAllStringSubmatch(string(raw), -1)
	if len(found) < 5 {
		t.Fatal("decision reasons not found")
	}
	for _, m := range found {
		if !refusalReason(m[1]) {
			t.Fatalf("decision reason %q is not in the diagnostic allowlist", m[1])
		}
	}
}

func TestDiagnosticStageNamesAMissingSandboxRuntimeClass(t *testing.T) {
	if got := DiagnosticStage(ControllerError{"sandbox-runtime-class"}); got != "sandbox-runtime-class" {
		t.Fatal(got)
	}
	if got := DiagnosticStage(ControllerError{"sandbox-other"}); got != "unknown" {
		t.Fatal(got)
	}
}

// failedCheck is a RuntimeClass check failure with a reason, as
// podtransport.CheckRuntimeClass returns it.
func failedCheck(t *testing.T, status int) error {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	defer server.Close()
	client, err := podtransport.NewClient(podtransport.Config{APIServer: server.URL, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), BearerToken: strings.Repeat("x", 32)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CheckRuntimeClass(context.Background(), "kata")
	return err
}

func TestDiagnosticNamesTheRuntimeClassReasonAndKeepsTheStop(t *testing.T) {
	check := failedCheck(t, 404)
	err := podtransport.WithRuntimeClassReason(ControllerError{"sandbox-runtime-class"}, check)
	if got := DiagnosticStage(err); got != "sandbox-runtime-class-not-found" {
		t.Fatal(got)
	}
	// Handoff after a refused claim still proceeds exactly as before.
	if !SandboxUnavailable(err) || !SandboxUnavailable(ControllerError{"sandbox-runtime-class"}) {
		t.Fatal("sandbox stop no longer recognized")
	}
	// A reason never attaches to another stage.
	if got := DiagnosticStage(podtransport.WithRuntimeClassReason(ControllerError{"queue"}, check)); got != "queue" {
		t.Fatal(got)
	}
}

func TestDiagnosticNamesTheRepositoryTestsStep(t *testing.T) {
	for _, stage := range []string{
		"runs-deadline", "runs-renew", "runs-context", "candidate-import-renew", "candidate-import-fence", "candidate-unimported-success",
		"tests-observe-request", "baseline-observe-container", "candidate-import-transfer-timeout", "candidate-import-before-observe-request",
		"candidate-ready-after-state", "candidate-import-receipt",
	} {
		if got := DiagnosticStage(ControllerError{stage}); got != stage {
			t.Fatal("step not named", stage, got)
		}
		if got := DiagnosticStage(cleanupAfter{cause: ControllerError{"cleanup"}, stage: stage}); got != "cleanup-after-"+stage {
			t.Fatal("step lost after a failed cleanup", stage, got)
		}
	}
	for _, stage := range []string{"candidate-renew", "candidate-deadline", "runs-other", "reference-import-renew", "config-observe-request", "candidate-observe-other", "candidate-sensitive", "results-renew"} {
		if got := DiagnosticStage(ControllerError{stage}); got != "unknown" {
			t.Fatal("unbounded step named", stage, got)
		}
	}
	if testsStage("candidate", errors.New("other")) != "candidate" {
		t.Fatal("unrefined stage changed")
	}
}
