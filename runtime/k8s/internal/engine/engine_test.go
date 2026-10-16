package engine

import (
	"context"
	"testing"
)

func TestStubEngineAlwaysSucceeds(t *testing.T) {
	result, err := StubEngine{}.Invoke(context.Background(), Request{
		WorkItemID:         "wi-1",
		Repository:         "os-santiago/homedir",
		BaseRef:            "main",
		IssueNumbers:       []int{1573},
		AcceptanceCriteria: []string{"Issue is resolved."},
		AttemptNumber:      1,
	})
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	if !result.ValidationPassed {
		t.Fatal("ValidationPassed = false, want true")
	}
	if result.TimedOut {
		t.Fatal("TimedOut = true, want false")
	}
	if len(result.ChangedFiles) != 0 {
		t.Fatalf("ChangedFiles = %v, want empty", result.ChangedFiles)
	}
}

func TestEngineInterfaceIsSatisfiedByStubEngine(t *testing.T) {
	var _ Engine = StubEngine{}
}
