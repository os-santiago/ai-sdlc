package reviewflow

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/sourcefetch"
)

func TestReviewDiagnosticStageIsBounded(t *testing.T) {
	for err, want := range map[error]string{
		ControllerError{Stage: "readiness"}:                         "readiness",
		ControllerError{Stage: "sandbox-runtime-class"}:             "sandbox-runtime-class",
		fmt.Errorf("wrapped: %w", ControllerError{Stage: "pod"}):    "pod",
		ControllerError{Stage: "sensitive-fixture"}:                 "unknown",
		errors.New("sensitive-fixture"):                             "unknown",
		ErrReview:                                                   "rejected",
		ControllerError{Stage: "model-evidence"}:                    "model-evidence",
		ControllerError{Stage: "model-worker-exit-23"}:              "model-worker-exit-23",
		ControllerError{Stage: "model-worker-exit-256"}:             "model-unknown",
		ControllerError{Stage: "model-worker-exit-0"}:               "model-worker-exit-0",
		ControllerError{Stage: "model-worker-exit-00"}:              "model-unknown",
		ControllerError{Stage: "model-provider"}:                    "model-provider",
		ControllerError{Stage: "model-request-turn-budget"}:         "model-request-turn-budget",
		ControllerError{Stage: "model-request-binding"}:             "model-request-binding",
		ControllerError{Stage: "cleanup-pod"}:                       "cleanup-pod",
		ControllerError{Stage: "cleanup-pod-observe-timeout"}:       "cleanup-pod-observe-timeout",
		ControllerError{Stage: "cleanup-config-observe-status-5xx"}: "cleanup-config-observe-status-5xx",
		ControllerError{Stage: "model-mailbox-read-helper-exit-2"}:  "model-mailbox-read-helper-exit-2",
		ControllerError{Stage: "model-mailbox-read-sensitive"}:      "model-unknown",
		ControllerError{Stage: "cleanup-pod-sensitive"}:             "unknown",
		ControllerError{Stage: "preparation-source-rate-limited"}:   "preparation-source-rate-limited",
		ControllerError{Stage: "preparation-sensitive"}:             "unknown",

		// Mailbox read transport failures, by cause (issue #826).
		ControllerError{Stage: "model-mailbox-read-helper-transport-exec"}:     "model-mailbox-read-helper-transport-exec",
		ControllerError{Stage: "model-mailbox-read-helper-transport-relay"}:    "model-mailbox-read-helper-transport-relay",
		ControllerError{Stage: "model-mailbox-read-helper-transport-deadline"}: "model-mailbox-read-helper-transport-deadline",
		ControllerError{Stage: "model-mailbox-read-helper-transport-expired"}:  "model-mailbox-read-helper-transport-expired",
		ControllerError{Stage: "model-mailbox-read-helper-transport-other"}:    "model-unknown",
	} {
		if got := DiagnosticStage(err); got != want {
			t.Fatalf("%v: got %q, want %q", err, got, want)
		}
	}
	if got := DiagnosticStage(ControllerError{Stage: "model-sensitive-fixture"}); got == "model-sensitive-fixture" {
		t.Fatal("unbounded model stage")
	}
}

func TestSourceRateLimitStillStopsAsARejectedPreparation(t *testing.T) {
	if !errors.Is(errSourceRateLimited, ErrReview) {
		t.Fatal("a rate-limited preparation must stop like any other")
	}
}

func TestReviewDiagnosticNamesTheRuntimeClassReason(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"kind":"RuntimeClass","metadata":{"name":"kata"}}`))
	}))
	defer server.Close()
	client, err := podtransport.NewClient(podtransport.Config{APIServer: server.URL, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), BearerToken: strings.Repeat("x", 32)})
	if err != nil {
		t.Fatal(err)
	}
	_, check := client.CheckRuntimeClass(context.Background(), "kata")
	stopped := podtransport.WithRuntimeClassReason(ControllerError{Stage: "sandbox-runtime-class"}, check)
	if got := DiagnosticStage(stopped); got != "sandbox-runtime-class-no-handler" {
		t.Fatal(got)
	}
	// Verification still proceeds after a review refused before its claim.
	if !VerificationMayProceed("sandbox-runtime-class-unavailable", stopped) {
		t.Fatal("sandbox stop no longer recognized")
	}
}

func TestSourceCredentialFailuresAreNamedPreparationStops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.json")
	document := `{"version":1,"host":"github.com","repository":"fixture/product","access":"write","token":"github_pat_` + strings.Repeat("F", 82) + `"}`
	if err := os.WriteFile(path, []byte(document), 0o400); err != nil {
		t.Fatal(err)
	}
	reader, err := sourcefetch.InstalledReader(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime := Runtime{Source: reader}
	_, err = runtime.Source.Fetch(context.Background(), "fixture/product", strings.Repeat("a", 40))
	stop := sourceError(err)
	if !errors.Is(stop, ErrReview) || preparationStage(stop) != "preparation-source-credential-invalid" {
		t.Fatal("an unusable credential must stop preparation by name")
	}
	if strings.Contains(err.Error(), "github_pat_") || strings.Contains(stop.Error(), "github_pat_") {
		t.Fatal("credential in error")
	}
	for err, stage := range map[error]string{errSourceCredentialRejected: "preparation-source-credential-rejected", errSourceRateLimited: "preparation-source-rate-limited", ErrReview: "preparation"} {
		if !errors.Is(err, ErrReview) || preparationStage(err) != stage || DiagnosticStage(ControllerError{Stage: stage}) != stage {
			t.Fatal(stage)
		}
	}
	if sourceError(errors.New("other")) != ErrReview {
		t.Fatal("an unclassified source failure stays a rejected preparation")
	}
}
