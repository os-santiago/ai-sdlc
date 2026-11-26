package publicationflow

import (
	"errors"
	"fmt"
	"testing"
)

func TestDiagnosticStageIsBounded(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{ControllerError{Stage: "tree", Class: "status-422"}, "tree-status-422"},
		{ControllerError{Stage: "tree", Class: "transport"}, "tree-transport"},
		{ControllerError{Stage: "commit", Class: "mismatch"}, "commit-mismatch"},
		{ControllerError{Stage: "pull-request", Class: "status-5xx"}, "pull-request-status-5xx"},
		{ControllerError{Stage: "tree"}, "tree"},
		{ControllerError{Stage: "tree-plan"}, "tree-plan"},
		{fmt.Errorf("wrapped: %w", ControllerError{Stage: "branch", Class: "fence"}), "branch-fence"},
		// Anything outside the closed sets is never echoed.
		{ControllerError{Stage: "tree", Class: "status-<html>"}, "tree"},
		{ControllerError{Stage: "tree", Class: "secret response body"}, "tree"},
		{ControllerError{Stage: "private model output"}, "unknown"},
		{errors.New("credential text"), "unknown"},
	} {
		if got := DiagnosticStage(c.err); got != c.want {
			t.Errorf("DiagnosticStage(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
