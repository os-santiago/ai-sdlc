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
		Body:               "Optimize the existing community search query while preserving current behavior, response shape and validation; add a regression test covering the hot path.",
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
	issue.Body = "Change OIDC authentication roles and authorization behavior with appropriate tests, " +
		"covering both the role-assignment path and the token-validation middleware end to end."
	got := Evaluate(issue, DefaultPolicy())
	if got.Decision != NeedsHuman || got.Risk != "high" {
		t.Fatalf("got %#v", got)
	}
	found := false
	for _, g := range got.RequiredGates {
		if g == "human-security-review" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected human-security-review gate, got %v", got.RequiredGates)
	}
}

// Homedir os-santiago/homedir#1576 asked for browser layout regression tests
// and only excluded authorization changes under "Out of scope"; a declared
// exclusion is not a request and must not escalate the issue.
func TestEvaluateIgnoresExcludedSecurityTerms(t *testing.T) {
	issue := validIssue()
	issue.Title = "Add browser layout regression tests"
	issue.Body = "Add browser layout regression tests covering the homepage and focus " +
		"indicators so future markup changes cannot silently regress the rendered layout.\n\n" +
		"## Out of scope\n\n" +
		"- No authorization changes\n" +
		"- No login flow changes\n"
	got := Evaluate(issue, DefaultPolicy())
	if got.Decision != Accepted || got.Risk != "low" {
		t.Fatalf("got %#v", got)
	}
}

func TestEvaluateExclusionHeadingVariants(t *testing.T) {
	headings := []string{
		"## Out of scope",
		"### Out of scope",
		"## Non-goals",
		"## Not in scope",
		"## Exclusions",
	}
	for _, h := range headings {
		issue := validIssue()
		issue.Title = "Add browser layout regression tests"
		issue.Body = "Add browser layout regression tests covering the homepage and focus " +
			"indicators so future markup changes cannot silently regress the rendered layout.\n\n" +
			h + "\n\n" +
			"- No authorization or oidc changes\n"
		got := Evaluate(issue, DefaultPolicy())
		if got.Decision != Accepted || got.Risk != "low" {
			t.Fatalf("heading %q: got %#v", h, got)
		}
	}
}

// Exclusions remove terms from the reviewed scope but never disarm the
// destructive scan: an "out of scope" line that names destructive work is
// itself a stop signal.
func TestEvaluateRejectsDestructiveWorkInsideExclusion(t *testing.T) {
	issue := validIssue()
	issue.Title = "Add browser layout regression tests"
	issue.Body = "Add browser layout regression tests covering the homepage and focus " +
		"indicators so future markup changes cannot silently regress the rendered layout.\n\n" +
		"## Out of scope\n\n" +
		"- Do not wipe production data\n"
	got := Evaluate(issue, DefaultPolicy())
	if got.Decision != Rejected || got.Risk != "critical" {
		t.Fatalf("got %#v", got)
	}
}

// Only dedicated exclusion headings leave the reviewed scope: inline negated
// prose still names the boundary, so it keeps escalating conservatively.
func TestEvaluateNegatedClaimStillEscalates(t *testing.T) {
	issue := validIssue()
	issue.Title = "Adjust community board layout"
	issue.Body = "Adjust the community board layout and add regression coverage; " +
		"this change does not modify authorization in any way and keeps the current role checks intact."
	got := Evaluate(issue, DefaultPolicy())
	if got.Decision != NeedsHuman || got.Risk != "high" {
		t.Fatalf("got %#v", got)
	}
}

func TestEvaluateSecurityTermInKeptSectionHeadingStillEscalates(t *testing.T) {
	issue := validIssue()
	issue.Title = "Improve the community board layout"
	issue.Body = "Improve the community board layout and add regression coverage " +
		"so future markup changes cannot silently regress the rendered layout.\n\n" +
		"## OIDC provider migration\n\n" +
		"Implement the change described in the acceptance criteria.\n\n" +
		"## Out of scope\n\n" +
		"- No authorization changes\n"
	got := Evaluate(issue, DefaultPolicy())
	if got.Decision != NeedsHuman || got.Risk != "high" {
		t.Fatalf("got %#v", got)
	}
}

func TestEvaluateScopeMentioningHeadingStaysInScope(t *testing.T) {
	issue := validIssue()
	issue.Title = "Add browser layout regression tests"
	issue.Body = "Add browser layout regression tests covering the homepage and focus " +
		"indicators so future markup changes cannot silently regress the rendered layout.\n\n" +
		"## Non-goals and deferred authorization work\n\n" +
		"- The oidc provider swap is scheduled separately\n"
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

func TestPolicyFromContractIsUnboundedByDefault(t *testing.T) {
	policy := PolicyFromContract(contract.Default())
	if policy.MaximumAcceptanceCriteria != 0 {
		t.Fatalf("expected unbounded (0) acceptance-criteria ceiling by default, got %d", policy.MaximumAcceptanceCriteria)
	}

	issue := validIssue()
	issue.AcceptanceCriteria = make([]string, 50) // far beyond the DoR-recommended 8
	for i := range issue.AcceptanceCriteria {
		issue.AcceptanceCriteria[i] = "criterion"
	}
	got := Evaluate(issue, policy)
	if got.Decision == NeedsDecomposition {
		t.Fatalf("expected no decomposition ceiling by default (opt-in), got %#v", got)
	}
}

func TestPolicyFromContractEnforcesConfiguredCeiling(t *testing.T) {
	c := contract.Default()
	max := 3
	c.Triggers.AcceptanceCriteria.Max = &max
	policy := PolicyFromContract(c)

	issue := validIssue()
	issue.AcceptanceCriteria = []string{"one", "two", "three", "four"}
	got := Evaluate(issue, policy)
	if got.Decision != NeedsDecomposition {
		t.Fatalf("expected decomposition once the contract sets a ceiling, got %#v", got)
	}
}

func TestPolicyFromContractUsesMinBodyLength(t *testing.T) {
	policy := PolicyFromContract(contract.Default())
	if policy.MinimumBodyLength != 120 {
		t.Fatalf("MinimumBodyLength = %d, want 120 (spec/definition-of-ready.md R4)", policy.MinimumBodyLength)
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
