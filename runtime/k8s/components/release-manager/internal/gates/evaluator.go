// Package gates evaluates a PR's merge/release readiness against required
// checks, review feedback, issue-coverage and validation evidence — the
// ADR-0003 "publish/review" and "release" stage contracts.
//
// Ported from os-santiago/homedir-ai-sdlc (future-go/components/release-manager).
// ApplyContractDefaults is new: it sources MaximumRemediation from a repo's
// .ai-sdlc.yaml merge.max_repairs instead of a hardcoded constant — see
// docs/architecture/adr/0004-cloud-native-k8s-openshift-runtime.md.
package gates

import "github.com/os-santiago/ai-sdlc/runtime/k8s/internal/contract"

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
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Required   bool   `json:"required"`
}

type Snapshot struct {
	Checks                    []Check `json:"checks"`
	ActionableReviewCount     int     `json:"actionableReviewCount"`
	IssueCoveragePassed       bool    `json:"issueCoveragePassed"`
	ValidationEvidencePresent bool    `json:"validationEvidencePresent"`
	RemediationAttempts       int     `json:"remediationAttempts"`
	MaximumRemediation        int     `json:"maximumRemediation"`
	Merged                    bool    `json:"merged"`
	ReleaseStatus             string  `json:"releaseStatus"`
	ReleaseConclusion         string  `json:"releaseConclusion"`
}

type Result struct {
	State   State    `json:"state"`
	Reasons []string `json:"reasons"`
}

// ApplyContractDefaults fills snapshot.MaximumRemediation from the repo's
// .ai-sdlc.yaml merge.max_repairs (schema default: 3) when the caller left
// it unset, instead of future-go's hardcoded fallback of 5.
func ApplyContractDefaults(snapshot Snapshot, c contract.Contract) Snapshot {
	if snapshot.MaximumRemediation < 1 {
		snapshot.MaximumRemediation = c.MaxRepairsOrDefault()
	}
	return snapshot
}

func Evaluate(snapshot Snapshot) Result {
	if snapshot.MaximumRemediation < 1 {
		snapshot.MaximumRemediation = 3 // spec/ai-sdlc.schema.json merge.max_repairs default
	}
	if snapshot.Merged {
		if snapshot.ReleaseStatus != "completed" {
			return Result{State: Waiting, Reasons: []string{"Production release is pending."}}
		}
		if snapshot.ReleaseConclusion == "success" {
			return Result{State: Verified, Reasons: []string{"Production release completed successfully."}}
		}
		return Result{State: Escalated, Reasons: []string{"Production release failed after merge."}}
	}

	pending := make([]string, 0)
	failing := make([]string, 0)
	for _, check := range snapshot.Checks {
		if !check.Required {
			continue
		}
		switch check.Conclusion {
		case "success", "neutral", "skipped":
		case "failure", "error", "timed_out", "cancelled", "action_required":
			failing = append(failing, check.Name)
		default:
			pending = append(pending, check.Name)
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
