package executor

import (
	"context"
	"path/filepath"
)

// BaselineRef records the source baseline commit outside the checked-out
// branch (generatortools.BaselineRef).
const BaselineRef = "refs/ai-sdlc/baseline"

// InitializeBaseline is called only after VerifySource, before repository code
// executes. The launcher supplies fixed paths and a fresh, private emptyDir.
// The original source SHA remains assignment metadata, not this synthetic Git
// commit. Candidate provenance is verified externally against the source tree.
func InitializeBaseline(ctx context.Context, parent, emptyTemplate string) error {
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/nonexistent",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=AI-SDLC baseline", "GIT_AUTHOR_EMAIL=baseline@invalid",
		"GIT_COMMITTER_NAME=AI-SDLC baseline", "GIT_COMMITTER_EMAIL=baseline@invalid",
		"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
		"LC_ALL=C",
	}
	repo := filepath.Join(parent, "repository")
	prefix := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "commit.gpgSign=false", "-c", "core.autocrlf=false"}
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=ai-sdlc-baseline", "--template=" + emptyTemplate, "--separate-git-dir=" + filepath.Join(parent, "git"), "."},
		{"add", "--force", "--all", "--", "."},
		{"commit", "--quiet", "--allow-empty", "--no-gpg-sign", "-m", "AI-SDLC source baseline"},
		// The generator works on the checked-out branch and may commit there;
		// generator tools diff against this ref, which no checkout or commit
		// of the work branch moves.
		{"update-ref", BaselineRef, "HEAD"},
	} {
		command := append(append([]string{}, prefix...), args...)
		if runBounded(ctx, "/usr/bin/git", repo, env, command) != nil {
			return ErrProcess
		}
	}
	return nil
}
