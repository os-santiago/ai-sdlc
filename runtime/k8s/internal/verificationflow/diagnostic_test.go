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
