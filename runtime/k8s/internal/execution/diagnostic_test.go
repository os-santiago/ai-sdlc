package execution

import (
	"errors"
	"fmt"
	"testing"
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
