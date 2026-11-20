package repositorytests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

// MaxReportBytes bounds a JUnit report read from the workspace.
const MaxReportBytes = 4 * 1024 * 1024

// Runner executes a manifest's test command in an already prepared, offline
// workspace and produces exactly one bounded Run. It inherits no environment:
// the command sees only a fixed PATH, HOME and TMPDIR inside the workspace.
type Runner struct {
	Verification project.Verification
	Workspace    string // absolute path of the checked-out tree
	Scratch      string // absolute, writable path for HOME and TMPDIR
}

var errRunner = errors.New("repository test runner refused its input")

// Run executes the command once. A non-zero exit or a timeout is a result,
// not an error; only an invalid setup or an unreadable report is an error.
func (r Runner) Run(ctx context.Context) (Run, error) {
	v := r.Verification
	if len(v.Command) == 0 || !filepath.IsAbs(r.Workspace) || !filepath.IsAbs(r.Scratch) {
		return Run{}, errRunner
	}
	dir := r.Workspace
	if v.WorkingDir != "" {
		if project.CleanPath(v.WorkingDir) != nil {
			return Run{}, errRunner
		}
		dir = filepath.Join(r.Workspace, filepath.FromSlash(v.WorkingDir))
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(v.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, v.Command[0], v.Command[1:]...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + r.Scratch, "TMPDIR=" + r.Scratch, "LC_ALL=C.UTF-8", "TZ=UTC", "CI=1"}
	// Repository output is untrusted and never becomes evidence: discard it.
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	result := Run{}
	err := cmd.Run()
	switch {
	case err == nil:
		result.ExitCode = 0
	case ctx.Err() != nil:
		result.ExitCode = 124 // timed out
	default:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() < 0 || exit.ExitCode() > 255 {
			result.ExitCode = 125 // could not run, or killed by a signal
		} else {
			result.ExitCode = exit.ExitCode()
		}
	}
	if v.Result.JUnitPath != "" {
		tests, err := readReport(r.Workspace, v.Result.JUnitPath)
		if err != nil {
			return Run{}, err
		}
		result.Tests = tests
	}
	return result, nil
}

// readReport reads a JUnit report that must be a regular file inside the
// workspace (no symlink at any component). A missing report yields no tests.
func readReport(workspace, rel string) (map[string]string, error) {
	if project.CleanPath(rel) != nil {
		return nil, errRunner
	}
	path := workspace
	for _, part := range strings.Split(rel, "/") {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, errRunner
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errRunner
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxReportBytes {
		return nil, errRunner
	}
	return ParseJUnit(io.LimitReader(f, MaxReportBytes))
}

type junitCase struct {
	Name      string    `xml:"name,attr"`
	Classname string    `xml:"classname,attr"`
	Failure   *struct{} `xml:"failure"`
	Error     *struct{} `xml:"error"`
	Skipped   *struct{} `xml:"skipped"`
}

type junitSuite struct {
	Name   string       `xml:"name,attr"`
	Cases  []junitCase  `xml:"testcase"`
	Suites []junitSuite `xml:"testsuite"`
}

// ParseJUnit maps each test case to its status, keyed by the sha256 of its
// suite, class and name. Duplicate identities resolve to the worst status.
// More than MaxTests cases is refused.
func ParseJUnit(r io.Reader) (map[string]string, error) {
	decoder := xml.NewDecoder(r)
	decoder.Strict = true
	decoder.Entity = map[string]string{}
	var root struct {
		XMLName xml.Name
		junitSuite
	}
	if err := decoder.Decode(&root); err != nil || (root.XMLName.Local != "testsuites" && root.XMLName.Local != "testsuite") {
		return nil, errRunner
	}
	out := map[string]string{}
	rank := map[string]int{Pass: 0, Skip: 1, Fail: 2}
	var walk func(junitSuite, string) error
	walk = func(s junitSuite, prefix string) error {
		scope := prefix + "/" + s.Name
		for _, c := range s.Cases {
			status := Pass
			if c.Failure != nil || c.Error != nil {
				status = Fail
			} else if c.Skipped != nil {
				status = Skip
			}
			sum := sha256.Sum256([]byte(scope + "\x00" + c.Classname + "\x00" + c.Name))
			key := hex.EncodeToString(sum[:])
			if previous, ok := out[key]; !ok || rank[status] > rank[previous] {
				out[key] = status
			}
			if len(out) > MaxTests {
				return errRunner
			}
		}
		for _, child := range s.Suites {
			if err := walk(child, scope); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root.junitSuite, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// Encode renders a Run as the single result line ParseRun accepts.
func (r Run) Encode() []byte {
	raw, _ := json.Marshal(r)
	return raw
}
