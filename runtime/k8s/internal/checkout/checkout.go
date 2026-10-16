// Package checkout ensures a worker-owned directory is a clean checkout
// of a target repository at a ref, immediately before
// internal/engine.SCCEngine.Invoke runs — the second of the two explicit
// prerequisites that package's doc comment used to list as unbuilt.
//
// Mirrors .github/workflows/ai-sdlc-implement.yml's actions/checkout
// step: clone/fetch over HTTPS with a short-lived credential, reset to
// the exact ref, then strip the credential back out of the remote URL
// (persist-credentials: false in that workflow) before the engine ever
// runs — the engine executes on untrusted issue text in this same
// directory (spec/injection-defense.md) and must never find a
// push-capable credential sitting in .git/config.
package checkout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// TokenSource returns a short-lived credential for repository
// ("owner/name"). nil means anonymous — no credential is embedded, which
// only works for a public repo. The GitHub App-backed implementation
// this runtime wires in (components/worker/cmd/main.go, backed by
// internal/githubapp.App.InstallationToken) is deliberately the only one
// shipped: no PAT, no GITHUB_TOKEN fallback (CLAUDE.md "Critical
// Lessons"; ai-sdlc-implement.yml's fail-closed App-only posture).
type TokenSource func(ctx context.Context, repository string) (token string, err error)

// GitCheckout is the Checker internal/engine.SCCEngine.Checkout expects.
type GitCheckout struct {
	Tokens  TokenSource
	Binary  string // git binary; default "git"
	BaseURL string // default "https://github.com"; override in tests
}

func (g GitCheckout) binary() string {
	if g.Binary != "" {
		return g.Binary
	}
	return "git"
}

func (g GitCheckout) baseURL() string {
	if g.BaseURL != "" {
		return g.BaseURL
	}
	return "https://github.com"
}

func (g GitCheckout) remoteURL(repository string) string {
	return fmt.Sprintf("%s/%s.git", g.baseURL(), repository)
}

// authenticatedURL embeds an x-access-token credential into an HTTPS
// remote URL — the standard way to authenticate a GitHub App
// installation token over git-over-HTTPS. Non-HTTPS remotes (used by
// checkout_test.go's file:// fixtures) pass through unchanged: a
// credential is meaningless for a local transport.
func authenticatedURL(remoteURL, token string) string {
	const scheme = "https://"
	if token == "" || !strings.HasPrefix(remoteURL, scheme) {
		return remoteURL
	}
	return scheme + "x-access-token:" + token + "@" + strings.TrimPrefix(remoteURL, scheme)
}

// EnsureClean makes dir a clean checkout of repository at ref: idempotent
// and safe to call before every bounded-edit attempt (ADR-0003) — a
// pre-existing checkout is fetched/reset/cleaned in place rather than
// re-cloned, so repeat attempts on the same work item are fast and any
// stray files the engine left behind on a prior (failed) attempt are
// removed before the next one starts.
func (g GitCheckout) EnsureClean(ctx context.Context, repository, ref, dir string) error {
	if repository == "" || ref == "" || dir == "" {
		return errors.New("checkout: repository, ref and dir are all required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("checkout: create dir: %w", err)
	}

	var token string
	if g.Tokens != nil {
		t, err := g.Tokens(ctx, repository)
		if err != nil {
			return fmt.Errorf("checkout: mint credential for %s: %w", repository, err)
		}
		token = t
	}

	plainURL := g.remoteURL(repository)
	authURL := authenticatedURL(plainURL, token)

	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := g.run(ctx, dir, "init"); err != nil {
			return err
		}
		if err := g.run(ctx, dir, "remote", "add", "origin", plainURL); err != nil {
			return err
		}
	}

	// The credential is embedded only for the fetch below, and stripped
	// again even if fetch/checkout fails (defer runs regardless of the
	// return path) — never leave a push-capable token in .git/config.
	if err := g.run(ctx, dir, "remote", "set-url", "origin", authURL); err != nil {
		return err
	}
	defer func() { _ = g.run(ctx, dir, "remote", "set-url", "origin", plainURL) }()

	if err := g.run(ctx, dir, "fetch", "--depth", "1", "origin", ref); err != nil {
		return fmt.Errorf("checkout: fetch %s@%s: %w", repository, ref, err)
	}
	if err := g.run(ctx, dir, "checkout", "--force", "-B", ref, "FETCH_HEAD"); err != nil {
		return fmt.Errorf("checkout: checkout %s@%s: %w", repository, ref, err)
	}
	if err := g.run(ctx, dir, "clean", "-fdx"); err != nil {
		return fmt.Errorf("checkout: clean %s: %w", dir, err)
	}
	return nil
}

func (g GitCheckout) run(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, g.binary(), args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
