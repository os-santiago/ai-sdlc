package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repositorytests"
)

// Fixed paths of the offline repository-tests pod.
const (
	testsParent  = "/workspaces"
	testsUIDPath = "/var/run/ai-sdlc/pod/uid"
	readyWait    = 60 * time.Second
)

// runRepositoryTests is the entrypoint copied into an offline verification
// pod. It waits for the transfer sidecar's ready marker for this pod and
// tree, checks the imported tree, runs the repository's test command once
// and writes exactly one bounded result line to stdout. It opens no
// database, network or cluster connection and loads no signing identity.
//
//	component repository-tests-run <verification.json> <scratch> <source-sha> <tree-sha256>
func runRepositoryTests(ctx context.Context, args []string, out io.Writer, parent, uidPath string) error {
	if len(args) != 4 {
		return errors.New("usage: repository-tests-run <verification.json> <scratch> <source-sha> <tree-sha256>")
	}
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
	// Validate through a manifest wrapper so the runner accepts only settings
	// the reviewed manifest rules allow.
	check := project.Manifest{Version: project.ManifestVersion, Repository: "verification/settings", BaseBranch: "main",
		Verification: &v, Publication: project.Publication{Allow: []string{"**"}, MaxChangedFiles: 1, MaxChangedBytes: 1, MaxFileBytes: 1},
		Merge: project.Merge{RulesetID: 1, Required: []project.Check{{Context: "settings", AppID: 1}}}}
	if check.Validate() != nil {
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
	result, err := repositorytests.Runner{Verification: v, Workspace: filepath.Join(parent, "repository"), Scratch: args[1]}.Run(ctx)
	if err != nil {
		return err
	}
	_, err = out.Write(append(result.Encode(), '\n'))
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
// an init container can place the runner into a volume the toolchain image
// mounts read-only. The verifier image has no shell or cp.
//
//	component install-runner <dest>
func installRunner(args []string, self string) error {
	if len(args) != 1 || !filepath.IsAbs(args[0]) || filepath.Clean(args[0]) != args[0] {
		return errors.New("usage: install-runner <absolute-destination>")
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
const maxRunnerBytes = 256 * 1024 * 1024
