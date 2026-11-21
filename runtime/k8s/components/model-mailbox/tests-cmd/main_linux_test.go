package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repositorytests"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

const testsPodUID = "11111111-1111-4111-8111-111111111111"

type testsPod struct {
	parent, scratch, uidPath, settings, source, tree string
}

func newTestsPod(t *testing.T, v project.Verification) testsPod {
	t.Helper()
	dir := t.TempDir()
	p := testsPod{parent: filepath.Join(dir, "workspaces"), scratch: filepath.Join(dir, "tmp"), uidPath: filepath.Join(dir, "uid"), settings: filepath.Join(dir, "verification.json"), source: strings.Repeat("a", 40)}
	for _, d := range []string{p.parent, p.scratch} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	b := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: p.source, Files: []workspacebundle.File{{Path: "go.mod", Mode: 0o644, Data: []byte("module x\n")}}}
	if err := workspacebundle.Import(p.parent, b); err != nil {
		t.Fatal(err)
	}
	p.tree, _ = b.TreeDigest()
	raw, _ := json.Marshal(v)
	if err := os.WriteFile(p.settings, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.uidPath, []byte(testsPodUID+"\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p testsPod) args() []string { return []string{p.settings, p.scratch, p.source, p.tree, "1"} }

func (p testsPod) ready(t *testing.T) {
	t.Helper()
	if err := repositorytests.PublishReady(p.parent, repositorytests.Ready{Version: repositorytests.ReadyVersion, PodUID: testsPodUID, SourceSHA: p.source, TreeSHA256: p.tree}); err != nil {
		t.Fatal(err)
	}
}

func settings() project.Verification {
	return project.Verification{Image: "registry.example/go@sha256:" + strings.Repeat("a", 64), Command: []string{"/bin/sh", "-c", "test -f go.mod && exit 7"},
		TimeoutSeconds: 30, CPU: "1", Memory: "1Gi", EphemeralStorage: "1Gi", Network: "none", Result: project.Result{ExitCode: true},
		Repeat: 1, TestGlobs: []string{"**/*_test.go"}}
}

func TestRepositoryTestsEntrypointWritesOneResultLine(t *testing.T) {
	p := newTestsPod(t, settings())
	p.ready(t)
	var out bytes.Buffer
	if err := runRepositoryTests(context.Background(), p.args(), &out, p.parent, p.uidPath); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSuffix(out.String(), "\n")
	if strings.Contains(line, "\n") {
		t.Fatal("more than one line", out.String())
	}
	if run, err := repositorytests.ParseRun([]byte(line)); err != nil || run.ExitCode != 7 {
		t.Fatal("the command must run inside the imported repository", run, err)
	}
	for _, bad := range [][]string{p.args()[:4], append(p.args()[:4], "0"), append(p.args()[:4], "4"), append(p.args()[:4], "2")} {
		if err := runRepositoryTests(context.Background(), bad, &bytes.Buffer{}, p.parent, p.uidPath); err == nil {
			t.Fatal("wrong arguments accepted", bad[4:])
		}
	}
}

func TestRepeatedRunsEachGetAFreshTree(t *testing.T) {
	v := settings()
	v.Repeat = 3
	// Each run sees the pristine tree and then dirties it.
	v.Command = []string{"/bin/sh", "-c", "test ! -e dirty && touch dirty && exit 5"}
	p := newTestsPod(t, v)
	p.ready(t)
	args := p.args()
	args[4] = "3"
	var out bytes.Buffer
	if err := runRepositoryTests(context.Background(), args, &out, p.parent, p.uidPath); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatal("one line per run", out.String())
	}
	for _, line := range lines {
		if run, err := repositorytests.ParseRun([]byte(line)); err != nil || run.ExitCode != 5 {
			t.Fatal("a run did not see a fresh tree", line, err)
		}
	}
}

func TestRepositoryTestsEntrypointRefusesUnreviewedSettings(t *testing.T) {
	v := settings()
	v.Network = "default"
	p := newTestsPod(t, v)
	p.ready(t)
	if err := runRepositoryTests(context.Background(), p.args(), &bytes.Buffer{}, p.parent, p.uidPath); err == nil {
		t.Fatal("settings outside the reviewed rules accepted")
	}
}

func TestRepositoryTestsEntrypointRunsNothingBeforeTheMatchingMarker(t *testing.T) {
	v := settings()
	v.Command = []string{"/bin/sh", "-c", "touch ran"}
	p := newTestsPod(t, v)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := runRepositoryTests(ctx, p.args(), &bytes.Buffer{}, p.parent, p.uidPath); err == nil {
		t.Fatal("ran without a ready marker")
	}
	p.ready(t)
	wrong := p.args()
	wrong[3] = strings.Repeat("d", 64)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	if err := runRepositoryTests(ctx2, wrong, &bytes.Buffer{}, p.parent, p.uidPath); err == nil {
		t.Fatal("ran for another tree")
	}
	if err := os.WriteFile(filepath.Join(p.parent, "repository", "planted"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runRepositoryTests(context.Background(), p.args(), &bytes.Buffer{}, p.parent, p.uidPath); err == nil {
		t.Fatal("ran on a tree that no longer matches")
	}
	if _, err := os.Stat(filepath.Join(p.parent, "repository", "ran")); err == nil {
		t.Fatal("repository code ran before every check passed")
	}
}

func TestInstallRunnerCopiesOnceAndExecutable(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "component")
	if err := installRunner([]string{dest}, self); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dest)
	if err != nil || info.Mode().Perm() != 0o555 {
		t.Fatal("runner mode", info, err)
	}
	if installRunner([]string{dest}, self) == nil {
		t.Fatal("an existing runner was replaced")
	}
	for _, bad := range [][]string{{}, {"relative"}, {"/a/../b"}, {dest, dest}} {
		if installRunner(bad, self) == nil {
			t.Fatal("accepted", bad)
		}
	}
}

func TestPodIdentityMustBeSmallAndPresent(t *testing.T) {
	dir := t.TempDir()
	if _, err := readUID(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing identity accepted")
	}
	big := filepath.Join(dir, "uid")
	if err := os.WriteFile(big, []byte(strings.Repeat("a", 129)), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := readUID(big); err == nil {
		t.Fatal("oversized identity accepted")
	}
}
