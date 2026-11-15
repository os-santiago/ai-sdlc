// Package gates evaluates a PR's merge/release readiness against required
// checks, review feedback, issue-coverage and validation evidence — the
// ADR-0003 "publish/review" and "release" stage contracts.
//
// Ported from os-santiago/homedir-ai-sdlc (future-go/components/release-manager).
// ApplyContractDefaults is new: it sources MaximumRemediation from a repo's
// .ai-sdlc.yaml merge.max_repairs instead of a hardcoded constant — see
// docs/architecture/adr/0004-cloud-native-k8s-openshift-runtime.md.
package gates

import (
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/contract"
	"regexp"
	"strings"
)

var revision = regexp.MustCompile(`^[a-f0-9]{40}$`)

type State string

const (
	Waiting     State = "waiting"
	Remediation State = "remediation-required"
	Approved    State = "approved"
	Verified    State = "production-verified"
	Escalated   State = "needs-human"
)

type Check struct {
	Name       string `json:"name"`
	HeadSHA    string `json:"headSHA"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Required   bool   `json:"required"`
}

// Snapshot is a pure evaluator input, not authenticated evidence. A future
// collector must establish current forge identity and reviewed policy before
// using any result; these fields are not an HTTP authorization capability.
type Snapshot struct {
	AllReportedChecks         bool     `json:"allReportedChecks"`
	HeadSHA                   string   `json:"headSHA"`
	RequiredChecks            []string `json:"requiredChecks"`
	Checks                    []Check  `json:"checks"`
	ActionableReviewCount     int      `json:"actionableReviewCount"`
	IssueCoveragePassed       bool     `json:"issueCoveragePassed"`
	ValidationEvidencePresent bool     `json:"validationEvidencePresent"`
	RemediationAttempts       int      `json:"remediationAttempts"`
	MaximumRemediation        int      `json:"maximumRemediation"`
	Merged                    bool     `json:"merged"`
	ReleaseStatus             string   `json:"releaseStatus"`
	ReleaseConclusion         string   `json:"releaseConclusion"`
}

type Result struct {
	State   State    `json:"state"`
	Reasons []string `json:"reasons"`
}

// ApplyContractDefaults fills snapshot.MaximumRemediation from the repo's
// .ai-sdlc.yaml merge.max_repairs (schema default: 3) when the caller left
// it unset, instead of future-go's hardcoded fallback of 5.
func ApplyContractDefaults(snapshot Snapshot, c contract.Contract) Snapshot {
	if snapshot.MaximumRemediation == 0 {
		snapshot.MaximumRemediation = c.MaxRepairsOrDefault()
	}
	// An empty reviewed list means every reported check, not an unset floor.
	snapshot.AllReportedChecks = snapshot.AllReportedChecks || len(c.Verify.RequiredChecks) == 0
	// Named additions still matter when an added check has not been reported.
	// The reviewed contract is a floor; caller-supplied names can tighten it.
	combined := append([]string(nil), snapshot.RequiredChecks...)
	for _, name := range c.Verify.RequiredChecks {
		found := false
		for _, existing := range combined {
			if existing == name {
				found = true
				break
			}
		}
		if !found {
			combined = append(combined, name)
		}
	}
	snapshot.RequiredChecks = combined
	return snapshot
}

func Evaluate(snapshot Snapshot) Result {
	if snapshot.ActionableReviewCount < 0 || snapshot.RemediationAttempts < 0 || snapshot.MaximumRemediation < 0 || len(snapshot.Checks) > 1000 || len(snapshot.RequiredChecks) > 1000 {
		return Result{State: Escalated, Reasons: []string{"Gate evidence is invalid or exceeds bounds."}}
	}
	if snapshot.MaximumRemediation == 0 {
		snapshot.MaximumRemediation = 3 // spec/ai-sdlc.schema.json merge.max_repairs default
	}
	if snapshot.Merged {
		if snapshot.ReleaseStatus != "completed" {
			return Result{State: Waiting, Reasons: []string{"Production release is pending."}}
		}
		if snapshot.ReleaseConclusion == "success" {
			// A forge release result cannot prove the promoted digest, deployment
			// identity, user checks or rollback outcome. No legacy Snapshot can
			// mint a production-verified decision.
			return Result{State: Waiting, Reasons: []string{"Authenticated deployment and user-outcome evidence is required."}}
		}
		return Result{State: Escalated, Reasons: []string{"Production release failed after merge."}}
	}

	if !revision.MatchString(snapshot.HeadSHA) {
		return Result{State: Waiting, Reasons: []string{"Current pull-request head identity is required."}}
	}
	required := map[string]bool{}
	for _, name := range snapshot.RequiredChecks {
		if strings.TrimSpace(name) != name || name == "" || len(name) > 256 || required[name] {
			return Result{State: Escalated, Reasons: []string{"Required-check policy is ambiguous."}}
		}
		required[name] = true
	}
	pending := make([]string, 0)
	failing := make([]string, 0)
	observed := map[string]bool{}
	for _, check := range snapshot.Checks {
		if strings.TrimSpace(check.Name) != check.Name || check.Name == "" || len(check.Name) > 256 || observed[check.Name] {
			return Result{State: Escalated, Reasons: []string{"Check evidence has ambiguous identities."}}
		}
		observed[check.Name] = true
		// Empty policy means all reported checks, per the portable contract.
		if !snapshot.AllReportedChecks && len(required) > 0 && !required[check.Name] && !check.Required {
			continue
		}
		if check.HeadSHA != snapshot.HeadSHA || check.Status != "completed" {
			pending = append(pending, check.Name)
			continue
		}
		switch check.Conclusion {
		case "success":
		case "failure", "error", "timed_out", "cancelled", "action_required", "stale", "startup_failure":
			failing = append(failing, check.Name)
		default:
			// A skipped/neutral/unknown outcome does not prove the required
			// validation ran successfully. No implicit waiver is granted.
			pending = append(pending, check.Name)
		}
	}
	for name := range required {
		if !observed[name] {
			pending = append(pending, name)
		}
	}
	if len(failing) > 0 || snapshot.ActionableReviewCount > 0 ||
		!snapshot.IssueCoveragePassed || !snapshot.ValidationEvidencePresent {
		reasons := make([]string, 0, 4)
		if len(failing) > 0 {
			reasons = append(reasons, "Required checks are failing.")
		}
		if snapshot.ActionableReviewCount > 0 {
			reasons = append(reasons, "Actionable review feedback remains.")
		}
		if !snapshot.IssueCoveragePassed {
			reasons = append(reasons, "Issue coverage evidence is incomplete.")
		}
		if !snapshot.ValidationEvidencePresent {
			reasons = append(reasons, "Validation evidence is missing.")
		}
		if snapshot.RemediationAttempts >= snapshot.MaximumRemediation {
			return Result{State: Escalated, Reasons: append(reasons, "Maximum remediation attempts reached.")}
		}
		return Result{State: Remediation, Reasons: reasons}
	}
	if len(pending) > 0 || len(snapshot.Checks) == 0 {
		return Result{State: Waiting, Reasons: []string{"Required checks are pending."}}
	}
	return Result{State: Approved, Reasons: []string{"All required gates passed; normal repository merge rules still apply."}}
}
