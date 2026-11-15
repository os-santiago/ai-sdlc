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
	} {
		if got := DiagnosticStage(tc.err); got != tc.want {
			t.Fatalf("unexpected category %q", got)
		}
	}
}
