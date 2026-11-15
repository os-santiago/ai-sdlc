package intake

import "errors"

// Admission is an internal signed decision, never a claim that quality checks
// or release gates have passed. RequiredGates describes future requirements.
type Admission struct {
	Snapshot      Snapshot `json:"snapshot"`
	Decision      string   `json:"decision"`
	Risk          string   `json:"risk"`
	Reasons       []string `json:"reasons"`
	RequiredGates []string `json:"requiredGates"`
	PolicyVersion string   `json:"policyVersion"`
}

func (a Admission) ID() string { return "admission-" + a.Snapshot.ID() }
func (a Admission) Validate(c Config) error {
	if err := a.Snapshot.Validate(c); err != nil {
		return err
	}
	if a.PolicyVersion != "1.0.0" || len(a.Reasons) == 0 || len(a.Reasons) > 20 || len(a.RequiredGates) > 20 {
		return errors.New("invalid admission decision")
	}
	switch a.Decision {
	case "accepted", "rejected", "needs-clarification", "needs-human", "requires-decomposition":
	default:
		return errors.New("invalid admission outcome")
	}
	switch a.Risk {
	case "low", "medium", "high", "critical":
	default:
		return errors.New("invalid admission risk")
	}
	if a.Decision == "accepted" && (!a.Snapshot.Eligible || a.Risk != "low") {
		return errors.New("ineligible shadow admission")
	}
	return nil
}
