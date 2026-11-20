package repositorytests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

const testUID = "11111111-1111-4111-8111-111111111111"

func importedTree(t *testing.T) (string, string, string) {
	t.Helper()
	parent := t.TempDir()
	source := strings.Repeat("a", 40)
	b := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: source, Files: []workspacebundle.File{{Path: "go.mod", Mode: 0o644, Data: []byte("module x\n")}}}
	if err := workspacebundle.Import(parent, b); err != nil {
		t.Fatal(err)
	}
	tree, err := b.TreeDigest()
	if err != nil {
		t.Fatal(err)
	}
	return parent, source, tree
}

func TestReadyMarkerReleasesOnlyTheExpectedRun(t *testing.T) {
	parent, source, tree := importedTree(t)
	want := Ready{Version: ReadyVersion, PodUID: testUID, SourceSHA: source, TreeSHA256: tree}
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- WaitReady(ctx, parent, want)
	}()
	time.Sleep(50 * time.Millisecond)
	if err := PublishReady(parent, want); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal("waiter not released", err)
	}
	if PublishReady(parent, want) == nil {
		t.Fatal("an existing marker was replaced")
	}
	other := want
	other.TreeSHA256 = strings.Repeat("b", 64)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if WaitReady(ctx, parent, other) == nil {
		t.Fatal("a marker for another tree released the run")
	}
	if PublishReady(t.TempDir(), Ready{Version: ReadyVersion, PodUID: "x", SourceSHA: source, TreeSHA256: tree}) == nil {
		t.Fatal("invalid marker published")
	}
}

func TestWaitReadyIsBoundedAndRefusesNonRegularMarkers(t *testing.T) {
	parent, source, tree := importedTree(t)
	want := Ready{Version: ReadyVersion, PodUID: testUID, SourceSHA: source, TreeSHA256: tree}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if WaitReady(ctx, parent, want) == nil {
		t.Fatal("a missing marker released the run")
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(parent, "ready.json")); err != nil {
		t.Fatal(err)
	}
	if WaitReady(context.Background(), parent, want) == nil {
		t.Fatal("a symlinked marker accepted")
	}
	os.Remove(filepath.Join(parent, "ready.json"))
	if err := unixMkfifo(filepath.Join(parent, "ready.json")); err != nil {
		t.Fatal(err)
	}
	if WaitReady(context.Background(), parent, want) == nil {
		t.Fatal("a FIFO marker accepted")
	}
}

func TestVerifyTreeRefusesChangedOrGitTrees(t *testing.T) {
	parent, source, tree := importedTree(t)
	if err := VerifyTree(parent, source, tree); err != nil {
		t.Fatal(err)
	}
	if VerifyTree(parent, source, strings.Repeat("c", 64)) == nil {
		t.Fatal("wrong tree accepted")
	}
	if err := os.WriteFile(filepath.Join(parent, "repository", "extra"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if VerifyTree(parent, source, tree) == nil {
		t.Fatal("a changed tree accepted")
	}
	os.Remove(filepath.Join(parent, "repository", "extra"))
	if err := os.Mkdir(filepath.Join(parent, "repository", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if VerifyTree(parent, source, tree) == nil {
		t.Fatal("a root .git accepted")
	}
}
