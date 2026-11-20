package repositorytests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

func runner(t *testing.T, command []string, report string) (Runner, string) {
	t.Helper()
	workspace, scratch := t.TempDir(), t.TempDir()
	v := project.Verification{Command: command, TimeoutSeconds: 30, Result: project.Result{ExitCode: true, JUnitPath: report}}
	return Runner{Verification: v, Workspace: workspace, Scratch: scratch}, workspace
}

func TestRunnerReportsExitCodesAndCleanEnvironment(t *testing.T) {
	r, workspace := runner(t, []string{"/bin/sh", "-c", `test -z "$SECRET" && test "$HOME" = "$TMPDIR" && test "$(pwd)" = "` + "$PWD" + `" && exit 3`}, "")
	t.Setenv("SECRET", "must-not-leak")
	got, err := r.Run(context.Background())
	if err != nil || got.ExitCode != 3 {
		t.Fatal("exit code or environment", got, err)
	}
	_ = workspace
	r.Verification.Command = []string{"/bin/sh", "-c", "exit 0"}
	if got, err := r.Run(context.Background()); err != nil || got.ExitCode != 0 {
		t.Fatal("success", got, err)
	}
	r.Verification.Command = []string{"/nonexistent/binary"}
	if got, err := r.Run(context.Background()); err != nil || got.ExitCode != 125 {
		t.Fatal("unrunnable command must be a failing result", got, err)
	}
}

func TestRunnerTimesOutAndKillsTheProcessGroup(t *testing.T) {
	r, _ := runner(t, []string{"/bin/sh", "-c", "sleep 60 & sleep 60"}, "")
	r.Verification.TimeoutSeconds = 1
	started := time.Now()
	got, err := r.Run(context.Background())
	if err != nil || got.ExitCode != 124 || time.Since(started) > 15*time.Second {
		t.Fatal("timeout", got, err, time.Since(started))
	}
}

const report = `<?xml version="1.0"?>
<testsuites>
  <testsuite name="pkg">
    <testcase classname="pkg" name="TestAdd"/>
    <testcase classname="pkg" name="TestSub"><failure message="x"/></testcase>
    <testcase classname="pkg" name="TestMul"><skipped/></testcase>
    <testsuite name="nested"><testcase classname="pkg" name="TestAdd"><error/></testcase></testsuite>
  </testsuite>
</testsuites>`

func TestJUnitStatusesAreHashedAndNested(t *testing.T) {
	tests, err := ParseJUnit(strings.NewReader(report))
	if err != nil || len(tests) != 4 {
		t.Fatal("parse", tests, err)
	}
	counts := map[string]int{}
	for id, status := range tests {
		if !testID.MatchString(id) {
			t.Fatal("unhashed id", id)
		}
		counts[status]++
	}
	if counts[Pass] != 1 || counts[Fail] != 2 || counts[Skip] != 1 {
		t.Fatal("statuses", counts)
	}
	duplicate := `<testsuite name="s"><testcase classname="c" name="t"/><testcase classname="c" name="t"><failure/></testcase></testsuite>`
	if got, err := ParseJUnit(strings.NewReader(duplicate)); err != nil || len(got) != 1 {
		t.Fatal("duplicate", got, err)
	} else {
		for _, status := range got {
			if status != Fail {
				t.Fatal("a duplicate identity must keep its worst status")
			}
		}
	}
	for _, bad := range []string{``, `<html/>`, `<testsuite><testcase name="a">`, `<!DOCTYPE x [<!ENTITY e "x">]><testsuite name="&e;"/>`} {
		if _, err := ParseJUnit(strings.NewReader(bad)); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	var many strings.Builder
	many.WriteString(`<testsuite name="s">`)
	for i := 0; i <= MaxTests; i++ {
		many.WriteString(`<testcase classname="c" name="t` + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + `"/>`)
	}
	many.WriteString(`</testsuite>`)
	if _, err := ParseJUnit(strings.NewReader(many.String())); err == nil {
		t.Fatal("more than MaxTests accepted")
	}
}

func TestRunnerReadsOnlyARegularReportInsideTheWorkspace(t *testing.T) {
	r, workspace := runner(t, []string{"/bin/sh", "-c", "mkdir -p out && cat > out/report.xml <<'X'\n" + report + "\nX"}, "out/report.xml")
	got, err := r.Run(context.Background())
	if err != nil || got.ExitCode != 0 || len(got.Tests) != 4 {
		t.Fatal("report", got, err)
	}
	if parsed, err := ParseRun(got.Encode()); err != nil || len(parsed.Tests) != 4 {
		t.Fatal("encoded result must parse", err)
	}
	r.Verification.Command = []string{"/bin/sh", "-c", "exit 1"}
	if err := os.RemoveAll(filepath.Join(workspace, "out")); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Run(context.Background()); err != nil || got.ExitCode != 1 || len(got.Tests) != 0 {
		t.Fatal("a missing report yields no tests", got, err)
	}
	outside := filepath.Join(t.TempDir(), "report.xml")
	if err := os.WriteFile(outside, []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "out", "report.xml")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background()); err == nil {
		t.Fatal("a symlinked report was read")
	}
}
