package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/engine"
)

const InputDirectory = "/var/run/ai-sdlc/assignment"
const WorkspaceDirectory = "/workspaces"

// SCCConfiguration is deterministic and contains no provider credential. The
// endpoint is Pod-local; external model calls require a separate authority.
func SCCConfiguration(p Profile) []byte {
	raw, _ := json.Marshal(map[string]any{
		"model":       map[string]any{"provider": "openai-compatible", "baseUrl": "http://127.0.0.1:8091/v1", "apiKey": "offline-mailbox-placeholder", "model": p.Model, "temperature": 0.2, "maxTokens": p.MaxTokens, "stream": false},
		"permissions": map[string]any{"autoApprove": []string{"read_file", "list_dir", "search_text", "memory_read", "write_file", "edit_file", "run_shell", "git", "memory_write", "code_query", "repo_probe"}, "denyPaths": []string{".env", ".env.*", "**/*.key", "**/*.pem", "**/id_rsa*", "**/.ssh/*", "**/credentials*"}},
	})
	return raw
}

// readProjected reads only a fixed file from a trusted read-only projection.
// Kubernetes ConfigMap projections use symlinks, so no-follow is inappropriate
// here. Repository files must never be passed to this function.
func readProjected(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrInvalid
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > int64(limit) {
		return nil, ErrInvalid
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(limit+1)))
	if err != nil || len(b) > limit {
		return nil, ErrInvalid
	}
	return b, nil
}

// Run is the fixed PID-1 workload entry point. Its return code reports only SCC
// process completion, never acceptance, review, release or deployment success.
// The caller exits immediately; external observation of container termination
// is required before collecting a candidate, including on exit zero.
func Run(ctx context.Context, assignmentDigest string) int {
	raw, err := readProjected(InputDirectory+"/assignment.json", MaxAssignmentBytes)
	if err != nil {
		return 1
	}
	a, err := ParseAssignment(raw, assignmentDigest, time.Now())
	if err != nil {
		return 1
	}
	ctx, cancel := context.WithDeadline(ctx, time.UnixMilli(a.ExpiresAt))
	defer cancel()
	profileRaw, err := readProjected(InputDirectory+"/profile.json", MaxProfileBytes)
	if err != nil {
		return 1
	}
	p, err := ParseProfile(profileRaw, a.ProfileSHA256)
	if err != nil {
		return 1
	}
	policy, err := engine.ReadRequiredADevBundle(InputDirectory+"/adev-profile.yaml", p.ADevSHA256)
	if err != nil {
		return 1
	}
	config, err := readProjected(InputDirectory+"/scc-config.json", MaxProfileBytes)
	if err != nil || string(config) != string(SCCConfiguration(p)) {
		return 1
	}
	uid, err := readProjected("/var/run/ai-sdlc/pod/uid", 128)
	if err != nil {
		return 1
	}
	start := Start{"ai-sdlc.executor-start/v1", strings.TrimSpace(string(uid)), assignmentDigest, a.TreeSHA256}
	waitCtx, stopWait := context.WithTimeout(ctx, 60*time.Second)
	err = WaitStart(waitCtx, WorkspaceDirectory, start)
	stopWait()
	if err != nil {
		return 1
	}
	if _, err = ParseAssignment(raw, assignmentDigest, time.Now()); err != nil {
		return 1
	}
	if VerifySource(WorkspaceDirectory, a) != nil {
		return 1
	}
	for _, name := range []string{"home", "tmp", "run"} {
		if os.Mkdir(filepath.Join(WorkspaceDirectory, name), 0700) != nil {
			return 1
		}
	}
	baselineCtx, stopBaseline := context.WithTimeout(ctx, 20*time.Second)
	err = InitializeBaseline(baselineCtx, WorkspaceDirectory, "/opt/ai-sdlc/empty-git-template")
	stopBaseline()
	if err != nil {
		return 1
	}
	prompt := WorkspaceDirectory + "/run/prompt.md"
	if os.WriteFile(prompt, []byte(BuildPrompt(a, policy)), 0600) != nil {
		return 1
	}
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/workspaces/home", "TMPDIR=/workspaces/tmp", "LC_ALL=C.UTF-8", "NO_COLOR=1", "CI=1",
		"SC_CONFIG_PATH=" + InputDirectory + "/scc-config.json", "SC_BASE_URL=http://127.0.0.1:8091/v1", "SC_MODEL=" + p.Model,
		"NODE_OPTIONS=--max-old-space-size=256", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
	}
	runCtx, stopRun := context.WithTimeout(ctx, time.Duration(p.MaxSeconds)*time.Second)
	defer stopRun()
	err = runBounded(runCtx, "/usr/local/bin/scc", WorkspaceDirectory+"/repository", env, []string{
		"chat", "-yq", "--prompt-file", prompt, "--no-commit", "--audit-log", WorkspaceDirectory + "/run/audit.jsonl", "--summary-file", WorkspaceDirectory + "/run/summary.json", "--max-seconds", strconv.Itoa(p.MaxSeconds), "--max-steps", strconv.Itoa(p.MaxSteps),
	})
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch exit.ExitCode() {
		case 10, 20, 21, 22, 23:
			return exit.ExitCode()
		}
	}
	if runCtx.Err() != nil {
		return 124
	}
	return 1
}
