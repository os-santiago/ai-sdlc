package admission

import (
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/contract"
)

func validIssue() Issue {
	return Issue{
		Repository:         "os-santiago/homedir",
		Number:             123,
		Revision:           "revision-1",
		Title:              "Improve community search performance",
		Body:               "Optimize the existing query while preserving current behavior and validation.",
		AcceptanceCriteria: []string{"Search returns the same results", "Latency is covered by a regression test"},
		RequestedBy:        "contributor",
		AuthorizedBy:       "maintainer",
	}
}

func TestEvaluateAcceptsAtomicSafeIssue(t *testing.T) {
	got := Evaluate(validIssue(), DefaultPolicy())
	if got.Decision != Accepted || got.Risk != "low" {
		t.Fatalf("got %#v", got)
	}
}

func TestEvaluateRejectsProtectionBypass(t *testing.T) {
	issue := validIssue()
	issue.Body = "Please bypass branch protection and required reviews so changes merge immediately."
	got := Evaluate(issue, DefaultPolicy())
	if got.Decision != Rejected || got.Risk != "critical" {
		t.Fatalf("got %#v", got)
	}
}

func TestEvaluateRequiresDecomposition(t *testing.T) {
	issue := validIssue()
	issue.AcceptanceCriteria = []string{"one", "two", "three", "four", "five", "six", "seven", "eight", "nine"}
	got := Evaluate(issue, DefaultPolicy())
	if got.Decision != NeedsDecomposition {
		t.Fatalf("got %#v", got)
	}
}

func TestEvaluateEscalatesSecurityBoundary(t *testing.T) {
	issue := validIssue()
	issue.Body = "Change OIDC authentication roles and authorization behavior with appropriate tests."
	got := Evaluate(issue, DefaultPolicy())
	if got.Decision != NeedsHuman || got.Risk != "high" {
		t.Fatalf("got %#v", got)
	}
}

func TestPolicyFromContractRequiresAuthorizationOnlyWhenLabelersConfigured(t *testing.T) {
	c := contract.Default()
	policy := PolicyFromContract(c)
	if policy.RequireAuthorizedAdmission {
		t.Fatal("expected RequireAuthorizedAdmission=false when triggers.authorized_labelers is empty")
	}

	c.Triggers.AuthorizedLabelers = []string{"maintainer-handle"}
	policy = PolicyFromContract(c)
	if !policy.RequireAuthorizedAdmission {
		t.Fatal("expected RequireAuthorizedAdmission=true when triggers.authorized_labelers is set")
	}
}

func TestPolicyFromContractMergesRequiredChecksIntoGates(t *testing.T) {
	c := contract.Default()
	c.Verify.RequiredChecks = []string{"CI / events-service"}
	policy := PolicyFromContract(c)
	if policy.RequiredGates[0] != "CI / events-service" {
		t.Fatalf("expected contract required check first, got %v", policy.RequiredGates)
	}
	found := false
	for _, g := range policy.RequiredGates {
		if g == "tests" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected baseline gate 'tests' to still be present, got %v", policy.RequiredGates)
	}
}
