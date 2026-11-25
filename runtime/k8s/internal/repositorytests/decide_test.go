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

func TestWeakDecisionsIgnoreTimeoutsAndUnrunnableCrossRuns(t *testing.T) {
	v := project.Verification{Repeat: 1, TestGlobs: []string{"**/*_test.go"}}
	for _, code := range []int{124, 125} {
		e := Evidence{Baseline: Run{ExitCode: 0}, Cross: Run{ExitCode: code}, Candidate: []Run{{ExitCode: 0}},
			Changes: []Change{{Path: "a/a_test.go"}}}
		if d := Decide(v, e); d.Passed || d.Reason != "no-failing-test" {
			t.Fatal("cross exit", code, d)
		}
	}
}

func withAnalysis(v project.Verification, commands int) project.Verification {
	v.Analysis = &project.Analysis{Commands: make([][]string, commands), TimeoutSeconds: 60}
	for i := range v.Analysis.Commands {
		v.Analysis.Commands[i] = []string{"lint", strconv.Itoa(i)}
	}
	return v
}

func analysed(r Run, codes ...int) Run {
	r.Analysis = codes
	return r
}

func TestAnalysisRefusesOnlyNewFailures(t *testing.T) {
	for _, report := range []bool{true, false} {
		v := withAnalysis(verification(report, 2), 3)
		c := runs(2, candidate)
		c[0] = analysed(candidate, 0, 1, 2)
		b := baseline
		if !report {
			b = Run{ExitCode: 0}
		}
		// Command 0 passes on both; 1 fails on both (no signal); 2 newly fails.
		d := Decide(v, Evidence{Baseline: analysed(b, 0, 1, 0), Cross: cross, Candidate: c, Changes: changes})
		if d.Passed || d.Reason != "candidate-analysis" || d.Summary.AnalysisCommands != 3 || d.Summary.AnalysisBaselineFailing != 1 || d.Summary.AnalysisNewFailures != 1 {
			t.Fatal("new analysis failure", report, d)
		}
		// A candidate that fixes the baseline's finding and adds none passes.
		c[0] = analysed(candidate, 0, 0, 0)
		d = Decide(v, Evidence{Baseline: analysed(b, 0, 1, 0), Cross: cross, Candidate: c, Changes: changes})
		if !d.Passed || d.Reason != "" || d.Summary.AnalysisBaselineFailing != 1 || d.Summary.AnalysisNewFailures != 0 {
			t.Fatal("clean candidate", report, d)
		}
		// A timeout (124) or an analyzer that could not run (125) on the
		// candidate, after a clean baseline, is a new failure too.
		for _, code := range []int{124, 125} {
			c[0] = analysed(candidate, code, 0, 0)
			if d := Decide(v, Evidence{Baseline: analysed(b, 0, 0, 0), Cross: cross, Candidate: c, Changes: changes}); d.Reason != "candidate-analysis" {
				t.Fatal("analysis exit", code, d)
			}
		}
	}
}

func TestAnalysisNeverMasksAnEarlierTestRefusal(t *testing.T) {
	v := withAnalysis(verification(true, 2), 1)
	failed := runs(2, Run{ExitCode: 1, Tests: candidate.Tests})
	failed[0] = analysed(failed[0], 1)
	d := Decide(v, Evidence{Baseline: analysed(baseline, 0), Cross: cross, Candidate: failed, Changes: changes})
	if d.Reason != "candidate-failed" || d.Summary.AnalysisNewFailures != 1 {
		t.Fatal("test refusal must come first", d)
	}
}

func TestAnalysisEvidenceMustMatchTheSettings(t *testing.T) {
	with := withAnalysis(verification(true, 2), 2)
	ok := runs(2, candidate)
	ok[0] = analysed(candidate, 0, 0)
	repeated := runs(2, analysed(candidate, 0, 0))
	for name, c := range map[string]struct {
		v project.Verification
		e Evidence
	}{
		"missing on the baseline":   {with, Evidence{Baseline: baseline, Cross: cross, Candidate: ok, Changes: changes}},
		"missing on the candidate":  {with, Evidence{Baseline: analysed(baseline, 0, 0), Cross: cross, Candidate: runs(2, candidate), Changes: changes}},
		"too few codes":             {with, Evidence{Baseline: analysed(baseline, 0), Cross: cross, Candidate: ok, Changes: changes}},
		"on the cross run":          {with, Evidence{Baseline: analysed(baseline, 0, 0), Cross: analysed(cross, 0, 0), Candidate: ok, Changes: changes}},
		"on a repetition":           {with, Evidence{Baseline: analysed(baseline, 0, 0), Cross: cross, Candidate: repeated, Changes: changes}},
		"without analysis settings": {verification(true, 2), Evidence{Baseline: analysed(baseline, 0, 0), Cross: cross, Candidate: ok, Changes: changes}},
	} {
		if d := Decide(c.v, c.e); d.Passed || d.Reason != "incomplete-evidence" {
			t.Fatal(name, d)
		}
	}
	if d := Decide(with, Evidence{Baseline: analysed(baseline, 0, 0), Cross: cross, Candidate: ok, Changes: changes}); !d.Passed {
		t.Fatal("matching evidence", d)
	}
}

// Without analysis the summary encodes exactly as before, so decision digests
// and proofs of existing manifests are unchanged.
func TestSummaryWithoutAnalysisIsUnchanged(t *testing.T) {
	d := Decide(verification(true, 2), Evidence{Baseline: baseline, Cross: cross, Candidate: runs(2, candidate), Changes: changes})
	raw, _ := json.Marshal(d)
	if strings.Contains(string(raw), "analysis") {
		t.Fatal("absent analysis encoded", string(raw))
	}
	if string(candidate.Encode()) != `{"exitCode":0,"tests":{"`+id("a")+`":"pass","`+id("b")+`":"pass"}}` {
		t.Fatal("result line changed", string(candidate.Encode()))
	}
}

func TestParseRunBoundsAnalysisCodes(t *testing.T) {
	if r, err := ParseRun([]byte(`{"exitCode":0,"analysis":[0,1,124,255]}`)); err != nil || len(r.Analysis) != 4 {
		t.Fatal("valid analysis codes", r, err)
	}
	for _, bad := range []string{
		`{"exitCode":0,"analysis":[0,0,0,0,0]}`,
		`{"exitCode":0,"analysis":[256]}`,
		`{"exitCode":0,"analysis":[-1]}`,
		`{"exitCode":0,"analysis":["0"]}`,
		`{"exitCode":0,"analysis":{}}`,
	} {
		if _, err := ParseRun([]byte(bad)); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}
