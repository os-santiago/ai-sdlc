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
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

// Bounds of a JUnit report read from the workspace.
const (
	MaxReportBytes = 4 * 1024 * 1024
	MaxSuiteDepth  = 16
	MaxNameBytes   = 1024
)

// searchPath is the PATH the command and its lookup use unless the reviewed
// settings declare one. It covers the official toolchain images' locations.
var searchPath = []string{"/usr/local/go/bin", "/usr/local/cargo/bin", "/opt/java/openjdk/bin", "/usr/local/bin", "/usr/bin", "/bin"}

// Runner executes a manifest's test command in an already prepared, offline
// workspace and produces exactly one bounded Run. It inherits no environment:
// the command sees only a fixed PATH, HOME and TMPDIR inside the workspace.
type Runner struct {
	Verification project.Verification
	Workspace    string // absolute path of the checked-out tree
	Scratch      string // absolute, writable path for HOME and TMPDIR
	// ReapAll kills every other process of the pid namespace once the
	// command ends, before the report is read, so nothing the repository
	// started (even outside its process group) can still change it. Set only
	// when the runner is the namespace's init process.
	ReapAll bool
}

var errRunner = errors.New("repository test runner refused its input")

// Run executes the command once. A non-zero exit or a timeout is a result,
// not an error; only an invalid setup or an unreadable report is an error.
func (r Runner) Run(ctx context.Context) (Run, error) {
	v := r.Verification
	code, found, err := r.execute(ctx, v.Command, time.Duration(v.TimeoutSeconds)*time.Second)
	if err != nil {
		return Run{}, err
	}
	result := Run{ExitCode: code}
	if !found {
		// The command could not be found: there is no report to read.
		return result, nil
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

// Analyze executes each reviewed analysis command once, in order, each with
// the analysis timeout, and returns their exit codes (124 for a timeout, 125
// for a command that could not run). Like Run, the output is discarded and a
// failing command is a result, not an error.
func (r Runner) Analyze(ctx context.Context) ([]int, error) {
	a := r.Verification.Analysis
	if a == nil || len(a.Commands) == 0 {
		return nil, errRunner
	}
	codes := make([]int, 0, len(a.Commands))
	for _, command := range a.Commands {
		code, _, err := r.execute(ctx, command, time.Duration(a.TimeoutSeconds)*time.Second)
		if err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	return codes, nil
}

// lookup resolves a bare program name with the reviewed PATH, never the
// runner's own environment, or returns "".
func (r Runner) lookup(name string) string {
	for _, d := range r.searchPath() {
		if info, err := os.Stat(filepath.Join(d, name)); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return filepath.Join(d, name)
		}
	}
	return ""
}

func (r Runner) searchPath() []string {
	if reviewed := r.Verification.SearchPath(); reviewed != nil {
		return reviewed
	}
	return searchPath
}

// execute runs one reviewed argv in the workspace with the fixed environment
// and returns its exit code: 124 for a timeout, 125 when it could not run or
// was killed by a signal. found is false when a bare program name is not on
// the reviewed PATH (exit code 125, nothing ran).
func (r Runner) execute(ctx context.Context, argv []string, timeout time.Duration) (code int, found bool, err error) {
	v := r.Verification
	if len(argv) == 0 || !filepath.IsAbs(r.Workspace) || !filepath.IsAbs(r.Scratch) {
		return 0, false, errRunner
	}
	dir := r.Workspace
	if v.WorkingDir != "" {
		if project.CleanPath(v.WorkingDir) != nil || noSymlinks(r.Workspace, v.WorkingDir) != nil {
			return 0, false, errRunner
		}
		dir = filepath.Join(r.Workspace, filepath.FromSlash(v.WorkingDir))
	}
	program := argv[0]
	if !strings.Contains(program, "/") {
		if program = r.lookup(argv[0]); program == "" {
			return 125, false, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, argv[1:]...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + strings.Join(r.searchPath(), ":"), "HOME=" + r.Scratch, "TMPDIR=" + r.Scratch, "LC_ALL=C.UTF-8", "TZ=UTC", "CI=1"}
	names := make([]string, 0, len(v.Env))
	for name := range v.Env {
		if name != "PATH" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		cmd.Env = append(cmd.Env, name+"="+v.Env[name])
	}
	// Repository output is untrusted and never becomes evidence: it goes to
	// /dev/null, so no pipe can keep the runner waiting.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	if r.ReapAll && reapAll(5*time.Second) != nil {
		// Something the repository started survived: its result is untrusted.
		return 0, true, errRunner
	}
	switch {
	case err == nil:
		return 0, true, nil
	case ctx.Err() != nil:
		return 124, true, nil // timed out
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() < 0 || exit.ExitCode() > 255 {
		return 125, true, nil // could not run, or killed by a signal
	}
	return exit.ExitCode(), true, nil
}

// noSymlinks reports whether every component of rel under root exists and
// none is a symbolic link.
func noSymlinks(root, rel string) error {
	path := root
	for _, part := range strings.Split(rel, "/") {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errRunner
		}
	}
	return nil
}

// readReport opens a JUnit report component by component beneath the
// workspace with O_NOFOLLOW (no symlink anywhere, no race between check and
// open) and O_NONBLOCK (a FIFO or device cannot block), and reads it only if
// it is a regular file within the size bound. A missing report yields no tests.
func readReport(workspace, rel string) (map[string]string, error) {
	if project.CleanPath(rel) != nil {
		return nil, errRunner
	}
	dir, err := unix.Open(workspace, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errRunner
	}
	parts := strings.Split(rel, "/")
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(dir, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(dir)
		if err == unix.ENOENT {
			return map[string]string{}, nil
		}
		if err != nil {
			return nil, errRunner
		}
		dir = next
	}
	fd, err := unix.Openat(dir, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	unix.Close(dir)
	if err == unix.ENOENT {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, errRunner
	}
	f := os.NewFile(uintptr(fd), rel)
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

// ParseJUnit maps each test case to its status, keyed by a sha256 over the
// length-prefixed suite path, class and name (no two different triples share
// an identity). Duplicate identities resolve to the worst status. More than
// MaxTests cases (counted with duplicates), suites deeper than MaxSuiteDepth
// or names longer than MaxNameBytes are refused.
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
	cases := 0
	var walk func(junitSuite, [32]byte, int) error
	walk = func(s junitSuite, parent [32]byte, depth int) error {
		if depth > MaxSuiteDepth || len(s.Name) > MaxNameBytes {
			return errRunner
		}
		// A fixed-size digest per level keeps memory constant in depth.
		scope := sha256.Sum256(append(append(parent[:], framed(s.Name)...), 's'))
		for _, c := range s.Cases {
			cases++
			if cases > MaxTests || len(c.Name) > MaxNameBytes || len(c.Classname) > MaxNameBytes {
				return errRunner
			}
			status := Pass
			if c.Failure != nil || c.Error != nil {
				status = Fail
			} else if c.Skipped != nil {
				status = Skip
			}
			sum := sha256.Sum256(append(append(append(scope[:0:0], scope[:]...), framed(c.Classname)...), framed(c.Name)...))
			key := hex.EncodeToString(sum[:])
			if previous, ok := out[key]; !ok || rank[status] > rank[previous] {
				out[key] = status
			}
		}
		for _, child := range s.Suites {
			if err := walk(child, scope, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root.junitSuite, [32]byte{}, 0); err != nil {
		return nil, err
	}
	return out, nil
}

// framed length-prefixes a name so concatenations are unambiguous.
func framed(name string) []byte {
	return append([]byte(strconv.Itoa(len(name))+":"), name...)
}

// Encode renders a Run as the single result line ParseRun accepts.
func (r Run) Encode() []byte {
	raw, _ := json.Marshal(r)
	return raw
}

// reapAll kills and waits for every other process of the pid namespace (the
// runner is its init, so orphans are its children) until none is left. A
// process created during the cleanup gets the signal again on the next pass.
func reapAll(bound time.Duration) error {
	return reap(bound, func() { _ = syscall.Kill(-1, syscall.SIGKILL) })
}

// waitChildren waits, without signalling, until the caller has no children.
func waitChildren(bound time.Duration) error { return reap(bound, func() {}) }

func reap(bound time.Duration, signal func()) error {
	deadline := time.Now().Add(bound)
	for {
		signal()
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		switch {
		case err == syscall.ECHILD:
			return nil
		case err != nil && err != syscall.EINTR:
			return err
		case pid > 0:
			continue
		}
		if time.Now().After(deadline) {
			return errRunner
		}
		time.Sleep(10 * time.Millisecond)
	}
}
