package gates

import (
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/contract"
)

func greenSnapshot() Snapshot {
	return Snapshot{
		HeadSHA:        strings.Repeat("a", 40),
		RequiredChecks: []string{"tests", "security"},
		Checks: []Check{
			{Name: "tests", HeadSHA: strings.Repeat("a", 40), Status: "completed", Conclusion: "success", Required: true},
			{Name: "security", HeadSHA: strings.Repeat("a", 40), Status: "completed", Conclusion: "success", Required: true},
		},
		IssueCoveragePassed:       true,
		ValidationEvidencePresent: true,
		MaximumRemediation:        5,
	}
}

func TestEvaluateApprovesGreenPullRequest(t *testing.T) {
	got := Evaluate(greenSnapshot())
	if got.State != Approved {
		t.Fatalf("got %#v", got)
	}
}

func TestEvaluateRequestsRemediationForFailingCheck(t *testing.T) {
	snapshot := greenSnapshot()
	snapshot.Checks[0].Conclusion = "failure"
	got := Evaluate(snapshot)
	if got.State != Remediation {
		t.Fatalf("got %#v", got)
	}
}

func TestEvaluateEscalatesExhaustedRemediation(t *testing.T) {
	snapshot := greenSnapshot()
	snapshot.IssueCoveragePassed = false
	snapshot.RemediationAttempts = 5
	got := Evaluate(snapshot)
	if got.State != Escalated {
		t.Fatalf("got %#v", got)
	}
}

func TestEvaluateDoesNotTreatSuccessfulReleaseAsProductionProof(t *testing.T) {
	snapshot := Snapshot{Merged: true, ReleaseStatus: "completed", ReleaseConclusion: "success"}
	got := Evaluate(snapshot)
	if got.State != Waiting {
		t.Fatalf("got %#v", got)
	}
}

func TestApplyContractDefaultsUsesMaxRepairs(t *testing.T) {
	c := contract.Default()
	maxRepairs := 2
	c.Merge.MaxRepairs = &maxRepairs

	snapshot := ApplyContractDefaults(Snapshot{}, c)
	if snapshot.MaximumRemediation != 2 {
		t.Fatalf("MaximumRemediation = %d, want 2", snapshot.MaximumRemediation)
	}

	explicit := ApplyContractDefaults(Snapshot{MaximumRemediation: 7}, c)
	if explicit.MaximumRemediation != 7 {
		t.Fatalf("ApplyContractDefaults overrode an explicit value: %d", explicit.MaximumRemediation)
	}
}

func TestEvaluateRejectsIncompleteOrAmbiguousCheckEvidence(t *testing.T) {
	cases := map[string]func(*Snapshot){
		"missing head":             func(s *Snapshot) { s.HeadSHA = "" },
		"different head":           func(s *Snapshot) { s.Checks[0].HeadSHA = strings.Repeat("b", 40) },
		"running with old success": func(s *Snapshot) { s.Checks[0].Status = "in_progress" },
		"queued with old success":  func(s *Snapshot) { s.Checks[0].Status = "queued" },
		"missing required check":   func(s *Snapshot) { s.Checks = s.Checks[:1] },
		"only optional checks": func(s *Snapshot) {
			s.Checks = []Check{{Name: "optional", HeadSHA: s.HeadSHA, Status: "completed", Conclusion: "success"}}
		},
		"required flag omitted": func(s *Snapshot) { s.Checks[0].Required = false; s.Checks[0].Conclusion = "failure" },
		"duplicate name":        func(s *Snapshot) { s.Checks = append(s.Checks, s.Checks[0]) },
		"duplicate policy":      func(s *Snapshot) { s.RequiredChecks = append(s.RequiredChecks, "tests") },
		"empty policy name":     func(s *Snapshot) { s.RequiredChecks = []string{""} },
		"negative feedback":     func(s *Snapshot) { s.ActionableReviewCount = -1 },
		"negative attempts":     func(s *Snapshot) { s.RemediationAttempts = -1 },
		"negative maximum":      func(s *Snapshot) { s.MaximumRemediation = -1 },
		"no reported checks":    func(s *Snapshot) { s.RequiredChecks = nil; s.Checks = nil },
	}
	for _, conclusion := range []string{"skipped", "neutral", "unknown", "", "stale", "startup_failure"} {
		cases["conclusion "+conclusion] = func(s *Snapshot) { s.Checks[0].Conclusion = conclusion }
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := greenSnapshot()
			mutate(&s)
			r := Evaluate(s)
			if r.State == Approved || r.State == Verified {
				t.Fatalf("false success: %#v", r)
			}
		})
	}
}
func TestEmptyRequiredPolicyChecksEveryReportedCheck(t *testing.T) {
	s := greenSnapshot()
	s.RequiredChecks = nil
	for i := range s.Checks {
		s.Checks[i].Required = false
	}
	if Evaluate(s).State != Approved {
		t.Fatal("all reported successful checks rejected")
	}
	s.Checks[0].Conclusion = "failure"
	if Evaluate(s).State != Remediation {
		t.Fatal("optional failure ignored under all-checks policy")
	}
}
func TestContractRequiredChecksCannotBeDroppedOrAliased(t *testing.T) {
	c := contract.Default()
	c.Verify.RequiredChecks = []string{"security", "tests"}
	s := Snapshot{RequiredChecks: []string{"local", "tests"}}
	result := ApplyContractDefaults(s, c)
	if strings.Join(result.RequiredChecks, ",") != "local,tests,security" {
		t.Fatal("contract floor was lost")
	}
	result.RequiredChecks[0] = "mutated"
	if s.RequiredChecks[0] != "local" || c.Verify.RequiredChecks[0] != "security" {
		t.Fatal("caller policy slice mutated")
	}
	s.MaximumRemediation = -1
	if ApplyContractDefaults(s, c).MaximumRemediation != -1 {
		t.Fatal("invalid explicit maximum was hidden")
	}
}

func TestContractAllReportedFloorSurvivesCallerSubset(t *testing.T) {
	c := contract.Default()
	s := greenSnapshot()
	s.RequiredChecks = []string{"tests"}
	s.Checks[1].Required = false
	s.Checks[1].Conclusion = "failure"
	if Evaluate(ApplyContractDefaults(s, c)).State != Remediation {
		t.Fatal("caller narrowed all-reported floor")
	}
	s.Checks[1].Conclusion = "success"
	s.RequiredChecks = []string{"not-yet-reported"}
	if Evaluate(ApplyContractDefaults(s, c)).State != Waiting {
		t.Fatal("all-reported floor dropped missing caller requirement")
	}
}
