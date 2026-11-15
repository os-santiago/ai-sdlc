package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func TestBaselineIncludesIgnoredSourceAndDisablesAmbientGit(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		t.Fatal("real Git required for executor baseline test")
	}
	parent := t.TempDir()
	if err := os.Mkdir(filepath.Join(parent, "repository"), 0700); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(parent, "repository")
	for name, body := range map[string]string{".gitignore": "ignored.txt\n", "ignored.txt": "original\r\n", "normal.txt": "hello\n"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Ambient Git environment is excluded by the child's complete environment.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", "/must-not-be-used")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := InitializeBaseline(ctx, parent, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/git", "ls-files", "--error-unmatch", "ignored.txt")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ignored source absent from baseline: %v %s", err, out)
	}
	cmd = exec.Command("/usr/bin/git", "status", "--porcelain")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("baseline not clean: %v %s", err, out)
	}
	b, err := workspacebundle.Export(parent, "0123456789012345678901234567890123456789")
	if err != nil || len(b.Files) != 3 {
		t.Fatalf("Git metadata leaked into candidate: %v", err)
	}
}
