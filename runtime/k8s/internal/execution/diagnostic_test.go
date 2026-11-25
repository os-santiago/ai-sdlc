package execution

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

func TestDiagnosticStageNeverLogsUnderlyingContent(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{errors.New("private credential and model output"), "authority-or-storage"},
		{ControllerError{"private credential and model output"}, "unknown"},
		{ControllerError{"model-private credential and model output"}, "model-unknown"},
		{fmt.Errorf("private wrapper: %w", ControllerError{"model-publication-helper-transport"}), "model-publication-helper-transport"},
		{ControllerError{"cleanup"}, "cleanup"},
		{ControllerError{"readiness-toolchain-dynamic"}, "readiness-toolchain-dynamic"},
		{ControllerError{"readiness-toolchain-private path"}, "unknown"},
		{ControllerError{"readiness-init-terminated"}, "readiness-init-terminated"},
		{ControllerError{"readiness-init-unstable"}, "readiness-init-unstable"},
		{ControllerError{"readiness-containers-before-init"}, "readiness-containers-before-init"},
		{ControllerError{"readiness-request"}, "readiness-request"},
		{ControllerError{"readiness-deadline"}, "readiness-deadline"},
		{ControllerError{"readiness-bind-unstable"}, "readiness-bind-unstable"},
		{ControllerError{"readiness-init-created"}, "readiness-init-created"},
		{ControllerError{"readiness-containers-created"}, "readiness-containers-created"},
		{ControllerError{"readiness-uid"}, "readiness-uid"},
		{ControllerError{"readiness-init-private"}, "unknown"},
		{ControllerError{"sandbox-runtime-class"}, "sandbox-runtime-class"},
		{ControllerError{"sandbox-runtime-class-timeout"}, "unknown"},
	} {
		if got := DiagnosticStage(tc.err); got != tc.want {
			t.Fatalf("unexpected category %q", got)
		}
	}
}

func TestCleanupFailurePreservesPrimaryDiagnostic(t *testing.T) {
	primary := ControllerError{"model-publication-helper-transport"}
	combined := cleanupFailure{cause: primary}
	if DiagnosticStage(combined) != "model-publication-helper-transport" || !CleanupPending(combined) {
		t.Fatal("cleanup hid primary handoff failure")
	}
	if !errors.Is(combined, primary) {
		t.Fatal("primary error chain lost")
	}
	if DiagnosticStage(cleanupFailure{}) != "cleanup" || !CleanupPending(cleanupFailure{}) {
		t.Fatal("cleanup-only diagnostic")
	}
	if CleanupPending(primary) || CleanupPending(nil) {
		t.Fatal("unexpected cleanup failure")
	}
	for _, err := range []error{errors.New("secret-canary"), ControllerError{"secret-canary"}} {
		value := cleanupFailure{cause: fmt.Errorf("secret-wrapper: %w", err)}
		if got := DiagnosticStage(value); got != "unknown" && got != "authority-or-storage" {
			t.Fatal("underlying sensitive error leaked", got)
		}
	}
}

func TestDiagnosticStageNamesTheRuntimeClassReason(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }))
	defer server.Close()
	client, err := podtransport.NewClient(podtransport.Config{APIServer: server.URL, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), BearerToken: strings.Repeat("x", 32)})
	if err != nil {
		t.Fatal(err)
	}
	_, check := client.CheckRuntimeClass(context.Background(), "kata")
	stopped := podtransport.WithRuntimeClassReason(ControllerError{"sandbox-runtime-class"}, check)
	if got := DiagnosticStage(stopped); got != "sandbox-runtime-class-not-found" {
		t.Fatal(got)
	}
	var controlled ControllerError
	if !errors.As(stopped, &controlled) || controlled.Stage != "sandbox-runtime-class" {
		t.Fatal("stop changed")
	}
	if sandboxState(check) != "sandbox-runtime-class-unavailable" {
		t.Fatal("missing class named unknown")
	}
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
	_, check = client.CheckRuntimeClass(context.Background(), "kata")
	if sandboxState(check) != "sandbox-runtime-class-unknown" || DiagnosticStage(podtransport.WithRuntimeClassReason(ControllerError{"sandbox-runtime-class"}, check)) != "sandbox-runtime-class-server-error" {
		t.Fatal("transient class failure not named unknown", check)
	}
}
