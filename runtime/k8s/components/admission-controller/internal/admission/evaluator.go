// Package admission normalizes work-item intake and produces policy-backed
// admission decisions.
//
// Ported from os-santiago/homedir-ai-sdlc (future-go/components/admission-controller),
// which deliberately produces a pure, synchronous Evaluate(issue, policy)
// function kept free of HTTP/transport concerns — see
// docs/architecture/adr/0004-cloud-native-k8s-openshift-runtime.md for
// provenance. PolicyFromContract is new in this port: it derives defaults
// from a repo's .ai-sdlc.yaml (internal/contract) instead of the standalone
// policies/defaults/v1/admission.yaml future-go used, so the k8s/OpenShift
// control plane and the GHA-native runtime share one declarative contract.
package admission

import (
	"regexp"
	"strings"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/contract"
)

type Decision string

const (
	Accepted           Decision = "accepted"
	Rejected           Decision = "rejected"
	NeedsClarification Decision = "needs-clarification"
	NeedsHuman         Decision = "needs-human"
	NeedsDecomposition Decision = "requires-decomposition"
)

type Issue struct {
	Repository         string   `json:"repository"`
	Number             int      `json:"number"`
	Revision           string   `json:"revision"`
	Title              string   `json:"title"`
	Body               string   `json:"body"`
	AcceptanceCriteria []string `json:"acceptanceCriteria"`
	RequestedBy        string   `json:"requestedBy"`
	AuthorizedBy       string   `json:"authorizedBy"`
}

// Policy mirrors spec/definition-of-ready.md's machine-checkable rules
// (R1-R6) plus the atomicity bound (DoR: "1-8 criteria ... >8 probably not
// atomic"). minimumTitleLength/minimumBodyLength/maximumAcceptanceCriteria
// have no field in spec/ai-sdlc.schema.json today — a gap this port
// surfaces rather than invents; see the ADR's "Schema gaps found" section.
type Policy struct {
	Version                    string   `json:"version"`
	MinimumTitleLength         int      `json:"minimumTitleLength"`
	MinimumBodyLength          int      `json:"minimumBodyLength"`
	MaximumAcceptanceCriteria  int      `json:"maximumAcceptanceCriteria"`
	RequireAcceptanceCriteria  bool     `json:"requireAcceptanceCriteria"`
	RequireAuthorizedAdmission bool     `json:"requireAuthorizedAdmission"`
	RequiredGates              []string `json:"requiredGates"`
}

type Result struct {
	Decision      Decision `json:"decision"`
	Risk          string   `json:"risk"`
	Reasons       []string `json:"reasons"`
	RequiredGates []string `json:"requiredGates"`
	PolicyVersion string   `json:"policyVersion"`
}

var destructivePatterns = compileAll(
	`\bdelete\s+(all|prod|production|database|data|users?)\b`,
	`\bdrop\s+(database|table|schema)\b`,
	`\bwipe\s+(data|database|server|prod|production)\b`,
	`\bdisable\s+(auth|authentication|authorization|security|checks?)\b`,
	`\bbypass\s+(branch protection|rulesets?|reviews?|checks?|security)\b`,
	`\bexpose\s+(secret|token|password|key)\b`,
	`\bforce\s+push\b`,
	`\b--admin\b`,
)

var guardrailDegradationPatterns = compileAll(
	`\bremove\s+(tests?|validation|monitoring|logging)\b`,
	`\bignore\s+(security|checks?|tests?|lint|validation)\b`,
)

var elevatedRiskPatterns = compileAll(
	`\b(auth|authentication|authorization|oauth|oidc|permission|role)\b`,
	`\b(database migration|schema migration|personal data|pii)\b`,
	`\b(security policy|cryptography|encryption|secret rotation)\b`,
)

// DefaultPolicy returns the standalone (contract-free) policy, used only
// when no .ai-sdlc.yaml is available (e.g. local unit tests, dry-run CLI).
func DefaultPolicy() Policy {
	return Policy{
		Version:                    "1.0.0",
		MinimumTitleLength:         8,
		MinimumBodyLength:          30,
		MaximumAcceptanceCriteria:  8, // spec/definition-of-ready.md: "1-8 criteria"
		RequireAcceptanceCriteria:  true,
		RequireAuthorizedAdmission: true,
		RequiredGates:              []string{"tests", "quality", "security", "issue-coverage"},
	}
}

// PolicyFromContract derives admission policy from a repo's .ai-sdlc.yaml.
// Fields with a direct schema equivalent are sourced from the contract;
// fields with no schema equivalent (title/body length floors, the DoR
// atomicity bound) keep the DoR-documented defaults.
func PolicyFromContract(c contract.Contract) Policy {
	policy := DefaultPolicy()
	policy.Version = c.Spec
	policy.RequireAuthorizedAdmission = len(c.Triggers.AuthorizedLabelers) > 0
	if len(c.Verify.RequiredChecks) > 0 {
		gates := append([]string{}, c.Verify.RequiredChecks...)
		for _, g := range policy.RequiredGates {
			if !contains(gates, g) {
				gates = append(gates, g)
			}
		}
		policy.RequiredGates = gates
	}
	return policy
}

func contains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

func Evaluate(issue Issue, policy Policy) Result {
	result := Result{
		Decision:      Accepted,
		Risk:          "low",
		RequiredGates: policy.RequiredGates,
		PolicyVersion: policy.Version,
	}
	if result.RequiredGates == nil {
		result.RequiredGates = []string{"tests", "quality", "security", "issue-coverage"}
	}
	text := strings.ToLower(issue.Title + "\n" + issue.Body)

	if issue.Repository == "" || issue.Number < 1 || issue.Revision == "" {
		result.Decision = NeedsClarification
		result.Reasons = append(result.Reasons, "Repository, issue number and revision are required.")
	}
	if len(strings.TrimSpace(issue.Title)) < policy.MinimumTitleLength ||
		len(strings.TrimSpace(issue.Body)) < policy.MinimumBodyLength {
		result.Decision = NeedsClarification
		result.Reasons = append(result.Reasons, "Issue is too short to implement safely.")
	}
	if policy.RequireAcceptanceCriteria && len(issue.AcceptanceCriteria) == 0 {
		result.Decision = NeedsClarification
		result.Reasons = append(result.Reasons, "Explicit acceptance criteria are required.")
	}
	if policy.RequireAuthorizedAdmission && strings.TrimSpace(issue.AuthorizedBy) == "" {
		result.Decision = NeedsHuman
		result.Reasons = append(result.Reasons, "Admission requires an authorized actor.")
	}
	if matchesAny(destructivePatterns, text) {
		result.Decision = Rejected
		result.Risk = "critical"
		result.Reasons = append(result.Reasons, "Issue requests destructive, secret-disclosing or protection-bypass work.")
		return result
	}
	if matchesAny(guardrailDegradationPatterns, text) {
		result.Decision = NeedsHuman
		result.Risk = "high"
		result.Reasons = append(result.Reasons, "Issue may weaken operational or engineering guardrails.")
	}
	if len(issue.AcceptanceCriteria) > policy.MaximumAcceptanceCriteria {
		result.Decision = NeedsDecomposition
		result.Reasons = append(result.Reasons, "Issue exceeds the configured atomic work limit.")
	}
	if matchesAny(elevatedRiskPatterns, text) {
		result.Risk = "high"
		result.RequiredGates = append(result.RequiredGates, "human-security-review")
		if result.Decision == Accepted {
			result.Decision = NeedsHuman
			result.Reasons = append(result.Reasons, "Security or data boundary changes require human review.")
		}
	}
	if len(result.Reasons) == 0 {
		result.Reasons = []string{"No blocking admission risks detected."}
	}
	return result
}

func compileAll(patterns ...string) []*regexp.Regexp {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		compiled = append(compiled, regexp.MustCompile(pattern))
	}
	return compiled
}

func matchesAny(patterns []*regexp.Regexp, text string) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	return false
}
