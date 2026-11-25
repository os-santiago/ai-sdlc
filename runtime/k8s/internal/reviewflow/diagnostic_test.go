package reviewflow

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
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
		ControllerError{Stage: "cleanup-pod"}:                       "cleanup-pod",
		ControllerError{Stage: "cleanup-pod-observe-timeout"}:       "cleanup-pod-observe-timeout",
		ControllerError{Stage: "cleanup-config-observe-status-5xx"}: "cleanup-config-observe-status-5xx",
		ControllerError{Stage: "model-mailbox-read-helper-exit-2"}:  "model-mailbox-read-helper-exit-2",
		ControllerError{Stage: "model-mailbox-read-sensitive"}:      "model-unknown",
		ControllerError{Stage: "cleanup-pod-sensitive"}:             "unknown",
		ControllerError{Stage: "preparation-source-rate-limited"}:   "preparation-source-rate-limited",
		ControllerError{Stage: "preparation-sensitive"}:             "unknown",
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
