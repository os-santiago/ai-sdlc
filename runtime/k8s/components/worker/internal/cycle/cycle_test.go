package cycle

import (
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/contract"
)

func TestEvaluateCompletesValidatedChange(t *testing.T) {
	got, err := Evaluate(Attempt{
		Number: 1, ChangedFiles: []string{"service.go"}, ValidationPassed: true,
	}, DefaultPolicy())
	if err != nil || got.Outcome != Completed {
		t.Fatalf("got=%#v err=%v", got, err)
	}
}

func TestEvaluateRepromptsNoop(t *testing.T) {
	got, err := Evaluate(Attempt{Number: 1, ValidationPassed: true}, DefaultPolicy())
	if err != nil || got.Outcome != Continue || !got.Reprompt {
		t.Fatalf("got=%#v err=%v", got, err)
	}
}

func TestEvaluateEscalatesAfterMaximumAttempts(t *testing.T) {
	got, err := Evaluate(Attempt{Number: 5, ExitCode: 1}, DefaultPolicy())
	if err != nil || got.Outcome != Escalated || got.Reprompt {
		t.Fatalf("got=%#v err=%v", got, err)
	}
}

func TestEvaluateContainsOversizedChange(t *testing.T) {
	got, err := Evaluate(Attempt{
		Number: 1, ChangedFiles: []string{"1", "2", "3", "4", "5", "6"},
	}, DefaultPolicy())
	if err != nil || got.Outcome != Escalated {
		t.Fatalf("got=%#v err=%v", got, err)
	}
}

func TestPolicyFromContractIsUnboundedChangedFilesByDefault(t *testing.T) {
	policy := PolicyFromContract(contract.Default())
	if policy.MaximumChangedFiles != 0 {
		t.Fatalf("expected unbounded (0) changed-files ceiling by default, got %d", policy.MaximumChangedFiles)
	}

	got, err := Evaluate(Attempt{
		Number: 1, ChangedFiles: make([]string, 50), ValidationPassed: true,
	}, policy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Outcome == Escalated {
		t.Fatalf("expected no file-count ceiling by default (opt-in), got %#v", got)
	}
}

func TestPolicyFromContractEnforcesConfiguredChangedFilesCeiling(t *testing.T) {
	c := contract.Default()
	max := 3
	c.Review.MaxChangedFiles = &max
	policy := PolicyFromContract(c)

	got, err := Evaluate(Attempt{
		Number: 1, ChangedFiles: []string{"a", "b", "c", "d"}, ValidationPassed: true,
	}, policy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Outcome != Escalated {
		t.Fatalf("expected escalation once the contract sets a changed-files ceiling, got %#v", got)
	}
}

func TestPolicyFromContractUsesMaxFixIterations(t *testing.T) {
	c := contract.Default()
	iterations := 2
	c.Review.MaxFixIterations = &iterations
	policy := PolicyFromContract(c)
	if policy.MaximumAttempts != 2 {
		t.Fatalf("MaximumAttempts = %d, want 2", policy.MaximumAttempts)
	}

	got, err := Evaluate(Attempt{Number: 2, ExitCode: 1}, policy)
	if err != nil || got.Outcome != Escalated {
		t.Fatalf("expected escalation at the contract-configured bound, got=%#v err=%v", got, err)
	}
}
