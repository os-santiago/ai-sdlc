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

func TestAFIFOAtTheReportPathCannotBlockTheRunner(t *testing.T) {
	r, workspace := runner(t, []string{"/bin/sh", "-c", "exit 0"}, "out/report.xml")
	if err := os.MkdirAll(filepath.Join(workspace, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unixMkfifo(filepath.Join(workspace, "out", "report.xml")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := r.Run(context.Background()); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO report was accepted")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the runner blocked on a FIFO report")
	}
}

func TestReportsCannotExhaustCPUOrMemory(t *testing.T) {
	var repeated strings.Builder
	repeated.WriteString(`<testsuite name="s">`)
	for i := 0; i <= MaxTests; i++ {
		repeated.WriteString(`<testcase classname="c" name="same"/>`)
	}
	repeated.WriteString(`</testsuite>`)
	if _, err := ParseJUnit(strings.NewReader(repeated.String())); err == nil {
		t.Fatal("repeated cases beyond MaxTests accepted")
	}
	deep := strings.Repeat(`<testsuite name="n">`, MaxSuiteDepth+2) + strings.Repeat(`</testsuite>`, MaxSuiteDepth+2)
	if _, err := ParseJUnit(strings.NewReader(deep)); err == nil {
		t.Fatal("suites deeper than MaxSuiteDepth accepted")
	}
	long := `<testsuite name="` + strings.Repeat("x", MaxNameBytes+1) + `"><testcase name="t"/></testsuite>`
	if _, err := ParseJUnit(strings.NewReader(long)); err == nil {
		t.Fatal("over-long name accepted")
	}
}

func TestDifferentSuitePathsNeverShareAnIdentity(t *testing.T) {
	flat, err := ParseJUnit(strings.NewReader(`<testsuite name="a/b"><testcase classname="c" name="t"/></testsuite>`))
	if err != nil {
		t.Fatal(err)
	}
	nested, err := ParseJUnit(strings.NewReader(`<testsuite name="a"><testsuite name="b"><testcase classname="c" name="t"/></testsuite></testsuite>`))
	if err != nil {
		t.Fatal(err)
	}
	for id := range flat {
		if _, collide := nested[id]; collide {
			t.Fatal("suite a/b and nested a then b share an identity")
		}
	}
}

func TestCommandLookupWorkingDirectoryAndProcessGroupKill(t *testing.T) {
	r, workspace := runner(t, []string{"sh", "-c", `test "$(pwd)" = "$EXPECTED" 2>/dev/null; echo $$ > "$HOME/pid"; exit 0`}, "")
	if got, err := r.Run(context.Background()); err != nil || got.ExitCode != 0 {
		t.Fatal("bare command resolved with the fixed PATH", got, err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "pkg"), 0o700); err != nil {
		t.Fatal(err)
	}
	r.Verification.WorkingDir = "pkg"
	r.Verification.Command = []string{"/bin/sh", "-c", `test "$(basename "$(pwd)")" = pkg`}
	if got, err := r.Run(context.Background()); err != nil || got.ExitCode != 0 {
		t.Fatal("working directory", got, err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(workspace, "link")); err != nil {
		t.Fatal(err)
	}
	r.Verification.WorkingDir = "link"
	if _, err := r.Run(context.Background()); err == nil {
		t.Fatal("symlinked working directory accepted")
	}
	r.Verification.WorkingDir = ""
	r.Verification.TimeoutSeconds = 1
	r.Verification.Command = []string{"/bin/sh", "-c", `sleep 60 & echo $! > "$HOME/child"; wait`}
	if got, err := r.Run(context.Background()); err != nil || got.ExitCode != 124 {
		t.Fatal("timeout", got, err)
	}
	raw, err := os.ReadFile(filepath.Join(r.Scratch, "child"))
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(raw))
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat("/proc/" + pid); err == nil {
		state, _ := os.ReadFile("/proc/" + pid + "/stat")
		if !strings.Contains(string(state), ") Z") {
			t.Fatal("background child survived the process-group kill", pid)
		}
	}
}

func TestRunnerUsesTheReviewedEnvironment(t *testing.T) {
	r, _ := runner(t, []string{"sh", "-c", `test "$GOFLAGS" = "-mod=vendor" && test "$PATH" = "/usr/bin:/bin" && test "$HOME" = "$TMPDIR"`}, "")
	r.Verification.Env = map[string]string{"PATH": "/usr/bin:/bin", "GOFLAGS": "-mod=vendor"}
	if got, err := r.Run(context.Background()); err != nil || got.ExitCode != 0 {
		t.Fatal("reviewed environment", got, err)
	}
	r.Verification.Env = map[string]string{"PATH": "/nonexistent"}
	if got, err := r.Run(context.Background()); err != nil || got.ExitCode != 125 {
		t.Fatal("lookup must use the reviewed PATH", got, err)
	}
}

func TestReapAllWaitsForEveryChild(t *testing.T) {
	// Outside a pid namespace kill(-1) would reach unrelated processes, so the
	// loop is exercised only for its wait semantics: with no children left it
	// returns at once.
	if os.Getpid() == 1 {
		t.Skip("only meaningful outside a pid namespace")
	}
	done := make(chan error, 1)
	go func() { done <- waitChildren(time.Second) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiting for no children blocked")
	}
}
