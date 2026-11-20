// Package repositorytests decides behaviour verification from a repository's
// own test suite, run by the sandboxed verifier against three trees:
//
//   - baseline:  the base revision;
//   - cross:     the base revision with only the candidate's test files;
//   - candidate: the candidate revision, run Repeat times.
//
// It is pure: it reads bounded runner results and the candidate delta, never
// the network, the cluster or repository files.
package repositorytests

import (
	"encoding/json"
	"errors"
	"regexp"
	"sort"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

// Recipe names this verification plug-in in commands and manifests.
const Recipe = "repository-tests/v1"

// Bounds of one runner result line.
const (
	MaxResultBytes = 32 * 1024
	MaxTests       = 512
)

var (
	ErrResult = errors.New("invalid repository test result")
	testID    = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Status of one test case in a JUnit report.
const (
	Pass = "pass"
	Fail = "fail"
	Skip = "skip"
)

// Run is one runner result: the test command's exit code and, when the
// manifest declares a JUnit report, each test's status keyed by the sha256
// of its fully qualified name (names are repository data, never logged).
type Run struct {
	ExitCode int               `json:"exitCode"`
	Tests    map[string]string `json:"tests,omitempty"`
}

// ParseRun accepts exactly one bounded runner result; the exit code must be
// stated explicitly, so a missing value can never read as success.
func ParseRun(raw []byte) (Run, error) {
	var r Run
	var fields map[string]json.RawMessage
	if len(raw) == 0 || len(raw) > MaxResultBytes || authority.DecodeStrict(raw, &r) != nil ||
		json.Unmarshal(raw, &fields) != nil || fields["exitCode"] == nil || string(fields["exitCode"]) == "null" ||
		r.ExitCode < 0 || r.ExitCode > 255 || len(r.Tests) > MaxTests {
		return Run{}, ErrResult
	}
	for id, status := range r.Tests {
		if !testID.MatchString(id) || (status != Pass && status != Fail && status != Skip) {
			return Run{}, ErrResult
		}
	}
	return r, nil
}

// MaxSummaryPaths bounds the changed test files listed in a summary.
const MaxSummaryPaths = 64

// Change is one path of the candidate delta.
type Change struct {
	Path    string
	Deleted bool // the candidate removes the file (a rename is a delete and an add)
	Added   bool // the file does not exist at the base revision
}

// Evidence is everything a decision depends on.
type Evidence struct {
	Baseline  Run
	Cross     Run
	Candidate []Run
	Changes   []Change
}

// Decision is the verification outcome and its reviewable summary.
type Decision struct {
	Passed  bool    `json:"passed"`
	Reason  string  `json:"reason"`
	Summary Summary `json:"summary"`
}

// Summary is safe to show the independent reviewer and to bind into a proof.
type Summary struct {
	BaselineExitCode  int  `json:"baselineExitCode"`
	CrossExitCode     int  `json:"crossExitCode"`
	CandidateExitCode int  `json:"candidateExitCode"`
	CandidateRuns     int  `json:"candidateRuns"`
	TestReport        bool `json:"testReport"`
	// Weak marks a decision without a per-test report: it shows only that
	// the candidate's tests fail on the base code, so the reviewer must
	// judge whether they test the requested behaviour.
	Weak             bool     `json:"weak"`
	FailToPass       int      `json:"failToPass"`
	Regressions      int      `json:"regressions"`
	ChangedTestFiles []string `json:"changedTestFiles"` // sorted, unique, at most MaxSummaryPaths
	BaselineFailing  bool     `json:"baselineFailing"`
}

// Decide applies the reviewed rules. A non-pass decision carries one reason,
// checked in this order:
//
//	"incomplete-evidence": the number of candidate runs differs from Repeat;
//	"invalid-delta": a changed path is invalid or both added and deleted;
//	"harness-changed": the candidate changes a test harness file;
//	"test-deleted": the candidate deletes or renames a test file;
//	"no-test-change": the candidate changes no test file;
//	"uncertain": candidate runs disagree (flaky or nondeterministic);
//	"candidate-failed": a candidate run exits non-zero;
//	"no-cross-report": with a report, the cross run produced none;
//	"regression": a test passing on the baseline no longer passes;
//	"baseline-failing": without a report, the baseline does not pass;
//	"no-failing-test": no candidate test fails on the base code.
func Decide(v project.Verification, e Evidence) Decision {
	report := v.Result.JUnitPath != ""
	s := Summary{BaselineExitCode: e.Baseline.ExitCode, CrossExitCode: e.Cross.ExitCode, CandidateRuns: len(e.Candidate),
		TestReport: report, Weak: !report, BaselineFailing: e.Baseline.ExitCode != 0, ChangedTestFiles: []string{}}
	result := func(passed bool, reason string) Decision { return Decision{Passed: passed, Reason: reason, Summary: s} }
	if len(e.Candidate) == 0 || len(e.Candidate) != v.Repeat {
		return result(false, "incomplete-evidence")
	}
	s.CandidateExitCode = e.Candidate[0].ExitCode
	// The summary's test file list is computed from the whole delta first,
	// so it is canonical whatever the decision.
	tests, deleted, harness := map[string]bool{}, false, false
	for _, change := range e.Changes {
		if project.CleanPath(change.Path) != nil || (change.Added && change.Deleted) {
			return result(false, "invalid-delta")
		}
		if v.MatchesHarness(change.Path) {
			harness = true
		}
		if v.MatchesTest(change.Path) {
			if change.Deleted {
				deleted = true
			} else {
				tests[change.Path] = true
			}
		}
	}
	for path := range tests {
		s.ChangedTestFiles = append(s.ChangedTestFiles, path)
	}
	sort.Strings(s.ChangedTestFiles)
	if len(s.ChangedTestFiles) > MaxSummaryPaths {
		s.ChangedTestFiles = s.ChangedTestFiles[:MaxSummaryPaths]
	}
	switch {
	case harness:
		return result(false, "harness-changed")
	case deleted:
		return result(false, "test-deleted")
	case len(tests) == 0:
		return result(false, "no-test-change")
	}
	first := e.Candidate[0]
	for _, run := range e.Candidate[1:] {
		if run.ExitCode != first.ExitCode || !sameTests(run.Tests, first.Tests) {
			return result(false, "uncertain")
		}
	}
	if first.ExitCode != 0 {
		return result(false, "candidate-failed")
	}
	if report {
		if len(e.Cross.Tests) == 0 || len(first.Tests) == 0 {
			// Without a cross report (for example the base code failed to
			// compile with the new tests) nothing proves fail-to-pass.
			return result(false, "no-cross-report")
		}
		for id, status := range e.Baseline.Tests {
			if status == Pass && first.Tests[id] != Pass {
				s.Regressions++
			}
		}
		for id, status := range first.Tests {
			// Only an explicit failure on the base code counts: a test that
			// is missing or skipped there proves nothing.
			if status == Pass && e.Cross.Tests[id] == Fail {
				s.FailToPass++
			}
		}
		if s.Regressions > 0 {
			return result(false, "regression")
		}
		if s.FailToPass == 0 {
			return result(false, "no-failing-test")
		}
		return result(true, "")
	}
	// Without a report: the baseline must pass and the candidate's tests must
	// fail on the base code. The decision is marked weak for the reviewer.
	if e.Baseline.ExitCode != 0 {
		return result(false, "baseline-failing")
	}
	if e.Cross.ExitCode == 0 {
		return result(false, "no-failing-test")
	}
	s.FailToPass = 1
	return result(true, "")
}

func sameTests(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for id, status := range a {
		if b[id] != status {
			return false
		}
	}
	return true
}

// Digest binds a decision and its summary into signed evidence.
func (d Decision) Digest() string {
	raw, _ := json.Marshal(d)
	return authority.Digest(raw)
}
