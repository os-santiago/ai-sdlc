package checkout

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStaticTokenAlwaysReturnsTheSameToken(t *testing.T) {
	ts := StaticToken("pat-abc123")
	for _, repository := range []string{"owner/repo", "other/thing", ""} {
		token, err := ts(context.Background(), repository)
		if err != nil {
			t.Fatalf("StaticToken()(%q) error = %v", repository, err)
		}
		if token != "pat-abc123" {
			t.Fatalf("StaticToken()(%q) = %q, want %q", repository, token, "pat-abc123")
		}
	}
}

func TestAuthenticatedURLEmbedsTokenForHTTPS(t *testing.T) {
	got := authenticatedURL("https://github.com/os-santiago/homedir.git", "ghs_abc123")
	want := "https://x-access-token:ghs_abc123@github.com/os-santiago/homedir.git"
	if got != want {
		t.Fatalf("authenticatedURL() = %q, want %q", got, want)
	}
}

func TestAuthenticatedURLLeavesNonHTTPSUnchanged(t *testing.T) {
	got := authenticatedURL("file:///tmp/repo.git", "ghs_abc123")
	if got != "file:///tmp/repo.git" {
		t.Fatalf("authenticatedURL() = %q, want unchanged", got)
	}
}

func TestAuthenticatedURLLeavesEmptyTokenUnchanged(t *testing.T) {
	got := authenticatedURL("https://github.com/os-santiago/homedir.git", "")
	if got != "https://github.com/os-santiago/homedir.git" {
		t.Fatalf("authenticatedURL() = %q, want unchanged", got)
	}
}

func TestEnsureCleanRequiresArgs(t *testing.T) {
	gc := GitCheckout{}
	if err := gc.EnsureClean(t.Context(), "", "main", t.TempDir()); err == nil {
		t.Fatal("EnsureClean() with no repository, want error")
	}
	if err := gc.EnsureClean(t.Context(), "owner/repo", "", t.TempDir()); err == nil {
		t.Fatal("EnsureClean() with no ref, want error")
	}
	if err := gc.EnsureClean(t.Context(), "owner/repo", "main", ""); err == nil {
		t.Fatal("EnsureClean() with no dir, want error")
	}
}

// requireGit skips the test when the git binary is unavailable — this
// fixture is otherwise fully offline (a local bare repo, no network).
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// seedBareRepo creates a bare repo at bareDir with one commit on "main",
// via a throwaway seed working tree — fully offline, no GitHub, no
// network, so EnsureClean's git mechanics are tested deterministically.
func seedBareRepo(t *testing.T, bareDir string) {
	t.Helper()
	if err := os.MkdirAll(bareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, bareDir, "init", "--bare", "-b", "main")

	seed := t.TempDir()
	runGit(t, seed, "init", "-b", "main")
	runGit(t, seed, "config", "user.email", "test@example.com")
	runGit(t, seed, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "commit", "-m", "seed")
	runGit(t, seed, "remote", "add", "origin", bareDir)
	runGit(t, seed, "push", "origin", "main")
}

func TestEnsureCleanClonesFetchesAndCleans(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	originRoot := filepath.Join(root, "origin")
	seedBareRepo(t, filepath.Join(originRoot, "repo.git"))
	work := filepath.Join(root, "work")

	gc := GitCheckout{BaseURL: "file://" + filepath.ToSlash(originRoot)}
	if err := gc.EnsureClean(t.Context(), "repo", "main", work); err != nil {
		t.Fatalf("EnsureClean() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(work, "README.md"))
	if err != nil {
		t.Fatalf("expected checked-out README.md: %v", err)
	}
	// Normalize line endings — Windows git (core.autocrlf) may check this
	// out as CRLF; the content, not the line ending, is what matters here.
	if got := strings.ReplaceAll(string(data), "\r\n", "\n"); got != "hello\n" {
		t.Fatalf("README.md = %q", got)
	}

	// A prior (failed) attempt leaves stray files; the next EnsureClean
	// call must wipe them.
	if err := os.WriteFile(filepath.Join(work, "stray.txt"), []byte("leftover"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := gc.EnsureClean(t.Context(), "repo", "main", work); err != nil {
		t.Fatalf("second EnsureClean() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "stray.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected stray.txt removed by clean, stat err = %v", err)
	}

	// The remote URL must never retain an embedded credential.
	out, err := exec.Command("git", "-C", work, "remote", "get-url", "origin").CombinedOutput()
	if err != nil {
		t.Fatalf("git remote get-url: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "@") {
		t.Fatalf("remote URL retained embedded credentials: %s", out)
	}
}

func TestEnsureCleanInvokesTokenSourceWithContext(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	originRoot := filepath.Join(root, "origin")
	seedBareRepo(t, filepath.Join(originRoot, "repo.git"))
	work := filepath.Join(root, "work")

	var sawRepository string
	gc := GitCheckout{
		BaseURL: "file://" + filepath.ToSlash(originRoot),
		Tokens: func(_ context.Context, repository string) (string, error) {
			sawRepository = repository
			return "", nil
		},
	}
	if err := gc.EnsureClean(t.Context(), "repo", "main", work); err != nil {
		t.Fatalf("EnsureClean() error = %v", err)
	}
	if sawRepository != "repo" {
		t.Fatalf("TokenSource saw repository = %q, want %q", sawRepository, "repo")
	}
}

func TestEnsureCleanPropagatesTokenSourceError(t *testing.T) {
	gc := GitCheckout{
		Tokens: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("token source failed")
		},
	}
	if err := gc.EnsureClean(t.Context(), "owner/repo", "main", t.TempDir()); err == nil {
		t.Fatal("EnsureClean() with failing TokenSource, want error")
	}
}
