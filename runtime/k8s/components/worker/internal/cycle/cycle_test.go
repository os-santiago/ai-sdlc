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
