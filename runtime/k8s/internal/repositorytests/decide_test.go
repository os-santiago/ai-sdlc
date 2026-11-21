package repositorytests

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

func id(n string) string { return strings.Repeat(n, 64) }

func verification(report bool, repeat int) project.Verification {
	v := project.Verification{Repeat: repeat, TestGlobs: []string{"**/*_test.go"}, HarnessGlobs: []string{"go.mod", "Makefile", "testdata/**"},
		Result: project.Result{ExitCode: true}}
	if report {
		v.Result.JUnitPath = "report.xml"
	}
	return v
}

func runs(n int, r Run) []Run {
	out := make([]Run, n)
	for i := range out {
		out[i] = r
	}
	return out
}

var (
	baseline  = Run{ExitCode: 0, Tests: map[string]string{id("a"): Pass}}
	cross     = Run{ExitCode: 1, Tests: map[string]string{id("a"): Pass, id("b"): Fail}}
	candidate = Run{ExitCode: 0, Tests: map[string]string{id("a"): Pass, id("b"): Pass}}
	changes   = []Change{{Path: "add.go"}, {Path: "add_test.go"}}
)

func TestReportedFailToPassWithoutRegressionPasses(t *testing.T) {
	d := Decide(verification(true, 2), Evidence{Baseline: baseline, Cross: cross, Candidate: runs(2, candidate), Changes: changes})
	if !d.Passed || d.Summary.FailToPass != 1 || d.Summary.Regressions != 0 || d.Summary.Weak || len(d.Summary.ChangedTestFiles) != 1 {
		t.Fatal("decision", d)
	}
	if len(d.Digest()) != 64 {
		t.Fatal("digest")
	}
}

func TestEveryRefusalHasOneReason(t *testing.T) {
	flaky := Run{ExitCode: 0, Tests: map[string]string{id("a"): Pass, id("b"): Fail}}
	for want, e := range map[string]Evidence{
		"incomplete-evidence": {Baseline: baseline, Cross: cross, Candidate: runs(1, candidate), Changes: changes},
		"invalid-delta":       {Baseline: baseline, Cross: cross, Candidate: runs(2, candidate), Changes: append(changes, Change{Path: "./old_test.go", Deleted: true})},
		"harness-changed":     {Baseline: baseline, Cross: cross, Candidate: runs(2, candidate), Changes: append(changes, Change{Path: "Makefile"})},
		"test-deleted":        {Baseline: baseline, Cross: cross, Candidate: runs(2, candidate), Changes: append(changes, Change{Path: "old_test.go", Deleted: true})},
		"no-test-change":      {Baseline: baseline, Cross: cross, Candidate: runs(2, candidate), Changes: []Change{{Path: "add.go"}}},
		"uncertain":           {Baseline: baseline, Cross: cross, Candidate: []Run{candidate, flaky}, Changes: changes},
		"candidate-failed":    {Baseline: baseline, Cross: cross, Candidate: runs(2, Run{ExitCode: 1, Tests: candidate.Tests}), Changes: changes},
		"no-cross-report":     {Baseline: baseline, Cross: Run{ExitCode: 2}, Candidate: runs(2, candidate), Changes: changes},
		"regression":          {Baseline: Run{Tests: map[string]string{id("a"): Pass, id("c"): Pass}}, Cross: cross, Candidate: runs(2, candidate), Changes: changes},
		"no-failing-test":     {Baseline: baseline, Cross: Run{ExitCode: 0, Tests: candidate.Tests}, Candidate: runs(2, candidate), Changes: changes},
	} {
		d := Decide(verification(true, 2), e)
		if d.Passed || d.Reason != want {
			t.Fatal("expected", want, "got", d.Reason, d.Passed)
		}
	}
	contradictory := []Change{{Path: "add_test.go", Added: true, Deleted: true}}
	if d := Decide(verification(true, 1), Evidence{Cross: cross, Candidate: runs(1, candidate), Changes: contradictory}); d.Reason != "invalid-delta" {
		t.Fatal("added and deleted must be invalid", d.Reason)
	}
}

func TestMissingOrSkippedCrossTestsNeverCountAsFailToPass(t *testing.T) {
	// The base code failed to compile with the new test: cross reports only
	// the existing test, so the new test is missing there.
	compileFailure := Run{ExitCode: 2, Tests: map[string]string{id("a"): Pass}}
	if d := Decide(verification(true, 1), Evidence{Baseline: baseline, Cross: compileFailure, Candidate: runs(1, candidate), Changes: changes}); d.Passed || d.Reason != "no-failing-test" {
		t.Fatal("missing cross test counted", d)
	}
	skipped := Run{ExitCode: 0, Tests: map[string]string{id("a"): Pass, id("b"): Skip}}
	if d := Decide(verification(true, 1), Evidence{Baseline: baseline, Cross: skipped, Candidate: runs(1, candidate), Changes: changes}); d.Passed {
		t.Fatal("skipped cross test counted", d)
	}
}

func TestWithoutAReportTheBaselineMustPassAndTheCrossRunFail(t *testing.T) {
	v := verification(false, 1)
	tests := []Change{{Path: "pkg/add_test.go", Added: true}}
	if d := Decide(v, Evidence{Baseline: Run{ExitCode: 0}, Cross: Run{ExitCode: 2}, Candidate: []Run{{ExitCode: 0}}, Changes: tests}); !d.Passed || !d.Summary.Weak {
		t.Fatal("weak fail-to-pass refused or not marked", d)
	}
	if d := Decide(v, Evidence{Baseline: Run{ExitCode: 0}, Cross: Run{ExitCode: 0}, Candidate: []Run{{ExitCode: 0}}, Changes: tests}); d.Passed || d.Reason != "no-failing-test" {
		t.Fatal("a cross run that passes proves nothing", d)
	}
	if d := Decide(v, Evidence{Baseline: Run{ExitCode: 1}, Cross: Run{ExitCode: 1}, Candidate: []Run{{ExitCode: 0}}, Changes: tests}); d.Passed || d.Reason != "baseline-failing" {
		t.Fatal("a failing baseline makes a failing cross meaningless", d)
	}
}

func TestSummaryIsCanonicalWhateverTheOrder(t *testing.T) {
	a := []Change{{Path: "b_test.go"}, {Path: "a_test.go"}, {Path: "a.go"}}
	b := []Change{{Path: "a.go"}, {Path: "a_test.go"}, {Path: "b_test.go"}, {Path: "a_test.go"}}
	first := Decide(verification(true, 1), Evidence{Baseline: baseline, Cross: cross, Candidate: runs(1, candidate), Changes: a})
	second := Decide(verification(true, 1), Evidence{Baseline: baseline, Cross: cross, Candidate: runs(1, candidate), Changes: b})
	if first.Digest() != second.Digest() || strings.Join(first.Summary.ChangedTestFiles, ",") != "a_test.go,b_test.go" {
		t.Fatal("summary not canonical", first.Summary, second.Summary)
	}
}

func TestRunnerResultsAreStrictAndBounded(t *testing.T) {
	if _, err := ParseRun([]byte(`{"exitCode":0,"tests":{"` + id("a") + `":"pass"}}`)); err != nil {
		t.Fatal("valid result refused", err)
	}
	for _, bad := range []string{
		``, `{}`, `{"exitCode":null}`, `{"exitCode":-1}`, `{"exitCode":256}`, `{"exitCode":1.5}`, `{"exitCode":0,"extra":1}`,
		`{"exitCode":0,"tests":{"name":"pass"}}`, `{"exitCode":0,"tests":{"` + id("a") + `":"flaky"}}`,
		`{"exitCode":0}{"exitCode":1}`, `{"exitCode":0,"exitCode":1}`,
		`{"exitCode":0,"tests":{"` + id("a") + `":"pass","` + id("a") + `":"fail"}}`,
	} {
		if _, err := ParseRun([]byte(bad)); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	var many strings.Builder
	many.WriteString(`{"exitCode":0,"tests":{`)
	for i := 0; i <= MaxTests; i++ {
		if i > 0 {
			many.WriteString(",")
		}
		h := strings.Repeat("0", 60) + string("0123456789abcdef"[i%16]) + string("0123456789abcdef"[(i/16)%16]) + string("0123456789abcdef"[(i/256)%16]) + "0"
		many.WriteString(`"` + h + `":"pass"`)
	}
	many.WriteString(`}}`)
	if _, err := ParseRun([]byte(many.String())); err == nil {
		t.Fatal("more than MaxTests accepted")
	}
}

func TestAFullRunAtTheTestLimitFitsTheResultBound(t *testing.T) {
	r := Run{ExitCode: 1, Tests: map[string]string{}}
	for i := 0; len(r.Tests) < MaxTests; i++ {
		h := strings.Repeat("f", 56) + strings.Repeat("0", 8-len(strconvItoa(i))) + strconvItoa(i)
		r.Tests[h] = Fail
	}
	raw, _ := json.Marshal(r)
	if _, err := ParseRun(raw); err != nil {
		t.Fatal("a run at MaxTests does not fit MaxResultBytes", len(raw))
	}
}

func strconvItoa(i int) string { return strconv.Itoa(i) }
