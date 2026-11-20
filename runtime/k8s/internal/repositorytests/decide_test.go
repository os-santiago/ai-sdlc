package repositorytests

import (
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

func id(n string) string { return strings.Repeat(n, 64) }

func verification(report bool, repeat int) project.Verification {
	v := project.Verification{Repeat: repeat, TestGlobs: []string{"**/*_test.go"}, Result: project.Result{ExitCode: true}}
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

func TestReportedFailToPassWithoutRegressionPasses(t *testing.T) {
	baseline := Run{ExitCode: 0, Tests: map[string]string{id("a"): Pass}}
	cross := Run{ExitCode: 1, Tests: map[string]string{id("a"): Pass, id("b"): Fail}}
	candidate := Run{ExitCode: 0, Tests: map[string]string{id("a"): Pass, id("b"): Pass}}
	d := Decide(verification(true, 2), Evidence{Baseline: baseline, Cross: cross, Candidate: runs(2, candidate),
		Changes: []Change{{Path: "add.go"}, {Path: "add_test.go"}}})
	if !d.Passed || d.Summary.FailToPass != 1 || d.Summary.Regressions != 0 || len(d.Summary.ChangedTestFiles) != 1 {
		t.Fatal("decision", d)
	}
	if len(d.Digest()) != 64 {
		t.Fatal("digest")
	}
}

func TestEveryRefusalHasOneReason(t *testing.T) {
	pass := Run{ExitCode: 0, Tests: map[string]string{id("a"): Pass, id("b"): Pass}}
	baseline := Run{ExitCode: 0, Tests: map[string]string{id("a"): Pass}}
	cross := Run{ExitCode: 1, Tests: map[string]string{id("a"): Pass, id("b"): Fail}}
	tests := []Change{{Path: "add.go"}, {Path: "add_test.go"}}
	for want, e := range map[string]Evidence{
		"uncertain":        {Baseline: baseline, Cross: cross, Candidate: []Run{pass, {ExitCode: 0, Tests: map[string]string{id("a"): Pass, id("b"): Fail}}}, Changes: tests},
		"candidate-failed": {Baseline: baseline, Cross: cross, Candidate: runs(2, Run{ExitCode: 1, Tests: pass.Tests}), Changes: tests},
		"regression":       {Baseline: Run{Tests: map[string]string{id("a"): Pass, id("c"): Pass}}, Cross: cross, Candidate: runs(2, pass), Changes: tests},
		"no-failing-test":  {Baseline: baseline, Cross: Run{ExitCode: 0, Tests: pass.Tests}, Candidate: runs(2, pass), Changes: tests},
		"no-test-change":   {Baseline: baseline, Cross: cross, Candidate: runs(2, pass), Changes: []Change{{Path: "add.go"}}},
		"test-deleted":     {Baseline: baseline, Cross: cross, Candidate: runs(2, pass), Changes: append(tests, Change{Path: "old_test.go", Deleted: true})},
	} {
		d := Decide(verification(true, 2), e)
		if d.Passed || d.Reason != want {
			t.Fatal("expected", want, "got", d.Reason, d.Passed)
		}
	}
	if d := Decide(verification(true, 2), Evidence{Baseline: baseline, Cross: cross, Candidate: runs(1, pass), Changes: tests}); d.Reason != "uncertain" {
		t.Fatal("missing candidate run must be uncertain", d.Reason)
	}
}

func TestWithoutAReportTheCrossRunMustFail(t *testing.T) {
	v := verification(false, 1)
	changes := []Change{{Path: "pkg/add_test.go", Added: true}}
	if d := Decide(v, Evidence{Cross: Run{ExitCode: 2}, Candidate: []Run{{ExitCode: 0}}, Changes: changes}); !d.Passed {
		t.Fatal("fail-to-pass without a report refused", d.Reason)
	}
	if d := Decide(v, Evidence{Cross: Run{ExitCode: 0}, Candidate: []Run{{ExitCode: 0}}, Changes: changes}); d.Passed || d.Reason != "no-failing-test" {
		t.Fatal("a cross run that passes proves nothing", d)
	}
	if d := Decide(v, Evidence{Baseline: Run{ExitCode: 1}, Cross: Run{ExitCode: 1}, Candidate: []Run{{ExitCode: 0}}, Changes: changes}); !d.Passed || !d.Summary.BaselineFailing {
		t.Fatal("a failing baseline is recorded, not blocking", d)
	}
}

func TestRunnerResultsAreStrictAndBounded(t *testing.T) {
	if _, err := ParseRun([]byte(`{"exitCode":0,"tests":{"` + id("a") + `":"pass"}}`)); err != nil {
		t.Fatal("valid result refused", err)
	}
	for _, bad := range []string{
		``, `{"exitCode":-1}`, `{"exitCode":256}`, `{"exitCode":0,"extra":1}`,
		`{"exitCode":0,"tests":{"name":"pass"}}`, `{"exitCode":0,"tests":{"` + id("a") + `":"flaky"}}`,
		`{"exitCode":0}{"exitCode":1}`, `{"exitCode":0,"exitCode":1}`,
		`{"exitCode":0,"tests":{` + strings.Repeat(`"`+id("a")+`":"pass",`, 2) + `"x":"y"}}`,
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
