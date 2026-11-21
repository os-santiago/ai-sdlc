package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repositorytests"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Fixed paths of the offline repository-tests pod.
const (
	testsParent  = "/workspaces"
	testsUIDPath = "/var/run/ai-sdlc/pod/uid"
	readyWait    = repositorytests.ReadySeconds * time.Second
)

// Command repository-tests ships in the model-mailbox image (static, no
// shell). An init container copies it into the offline test pod, where it
// runs inside the repository's pinned toolchain image:
//
//	repository-tests install <dest>
//	repository-tests run <verification.json> <scratch> <source-sha> <tree-sha256> <runs>
func main() {
	var err error
	switch {
	case len(os.Args) >= 2 && os.Args[1] == "install":
		err = installRunner(os.Args[2:], "/proc/self/exe")
	case len(os.Args) >= 2 && os.Args[1] == "run":
		err = runRepositoryTests(context.Background(), os.Args[2:], os.Stdout, testsParent, testsUIDPath)
	default:
		err = errors.New("usage: repository-tests install|run ...")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runRepositoryTests is the entrypoint copied into an offline verification
// pod. It waits for the transfer sidecar's ready marker for this pod and
// tree, checks the imported tree, runs the repository's test command <runs>
// times (each on a fresh copy of the verified tree, made before any run) and
// writes one bounded result line per run to stdout. It opens no database,
// network or cluster connection and loads no signing identity.
//
//	repository-tests run <verification.json> <scratch> <source-sha> <tree-sha256> <runs>
func runRepositoryTests(ctx context.Context, args []string, out io.Writer, parent, uidPath string) error {
	if len(args) != 5 || len(args[4]) != 1 || args[4][0] < '1' || args[4][0] > '3' {
		return errors.New("usage: repository-tests run <verification.json> <scratch> <source-sha> <tree-sha256> <runs 1-3>")
	}
	runs := int(args[4][0] - '0')
	f, err := os.Open(args[0])
	if err != nil {
		return errors.New("verification settings unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, project.MaxManifestBytes+1))
	if err != nil || len(raw) > project.MaxManifestBytes {
		return errors.New("verification settings unavailable")
	}
	var v project.Verification
	if authority.DecodeStrict(raw, &v) != nil {
		return errors.New("invalid verification settings")
	}
	if v.Validate() != nil || (runs != 1 && runs != v.Repeat) {
		return errors.New("invalid verification settings")
	}
	uid, err := readUID(uidPath)
	if err != nil {
		return err
	}
	expected := repositorytests.Ready{Version: repositorytests.ReadyVersion, PodUID: uid, SourceSHA: args[2], TreeSHA256: args[3]}
	wait, cancel := context.WithTimeout(ctx, readyWait)
	defer cancel()
	if repositorytests.WaitReady(wait, parent, expected) != nil {
		return errors.New("imported tree not ready")
	}
	if repositorytests.VerifyTree(parent, args[2], args[3]) != nil {
		return errors.New("imported tree does not match")
	}
	// A non-dumpable runner cannot be traced, and its /proc entries (and so
	// its stdout) cannot be opened, by the same-uid repository code; the flag
	// resets for the command on exec, which needs no protection.
	if unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) != nil {
		return errors.New("runner isolation unavailable")
	}
	// Every repetition gets its own copy of the verified tree, all made
	// before any repository code runs. A run can still alter later copies;
	// that can only make the repetitions disagree, which fails as uncertain.
	workspaces := []string{filepath.Join(parent, "repository")}
	if runs > 1 {
		tree, err := workspacebundle.Export(parent, args[2])
		if err != nil {
			return errors.New("imported tree does not match")
		}
		for i := 2; i <= runs; i++ {
			copyParent := filepath.Join(args[1], ".runs", strconv.Itoa(i))
			if os.MkdirAll(copyParent, 0o700) != nil || workspacebundle.Import(copyParent, tree) != nil {
				return errors.New("run copy failed")
			}
			workspaces = append(workspaces, filepath.Join(copyParent, "repository"))
		}
	}
	var lines []byte
	for _, workspace := range workspaces {
		runner := repositorytests.Runner{Verification: v, Workspace: workspace, Scratch: args[1], ReapAll: os.Getpid() == 1}
		result, err := runner.Run(ctx)
		if err != nil {
			return err
		}
		lines = append(append(lines, result.Encode()...), '\n')
	}
	// One write after every run, so a failure leaves no partial result set.
	_, err = out.Write(lines)
	return err
}

func readUID(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("pod identity unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 129))
	if err != nil || len(raw) > 128 {
		return "", errors.New("pod identity unavailable")
	}
	return strings.TrimSpace(string(raw)), nil
}

// installRunner copies this static binary to dest, which must not exist, so
// an init container of a restartPolicy Never pod (a failed copy fails the
// pod, never retries over a partial file) can place the runner into a volume
// the toolchain image mounts read-only. The mailbox image has no shell or cp.
//
//	repository-tests install <dest>
func installRunner(args []string, self string) error {
	if len(args) != 1 || !filepath.IsAbs(args[0]) || filepath.Clean(args[0]) != args[0] {
		return errors.New("usage: repository-tests install <absolute-destination>")
	}
	src, err := os.Open(self)
	if err != nil {
		return errors.New("runner binary unavailable")
	}
	defer src.Close()
	dst, err := os.OpenFile(args[0], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o555)
	if err != nil {
		return errors.New("runner destination unavailable")
	}
	_, copyErr := io.Copy(dst, io.LimitReader(src, maxRunnerBytes+1))
	syncErr := dst.Sync()
	closeErr := dst.Close()
	if info, err := os.Stat(args[0]); copyErr != nil || syncErr != nil || closeErr != nil || err != nil || info.Size() > maxRunnerBytes {
		os.Remove(args[0])
		return errors.New("runner copy failed")
	}
	return nil
}

// maxRunnerBytes bounds the copied binary.
const maxRunnerBytes = 96 * 1024 * 1024
