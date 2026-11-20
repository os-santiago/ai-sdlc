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
// noBytecode keeps repository commands (for example a Python test run) from
// writing __pycache__ files into the workspace; the candidate delta is the
// workspace, and recipes accept only the intended source changes. SCC hands
// run_shell children an allowlisted environment, so the name is allowlisted
// in its configuration as well.
const noBytecodeName, noBytecodeValue = "PYTHONDONTWRITEBYTECODE", "1"

func SCCConfiguration(p Profile) []byte {
	tools := []string{"read_file", "list_dir", "search_text", "memory_read", "write_file", "edit_file", "run_shell", "git", "memory_write", "code_query", "repo_probe"}
	if p.Version == ReviewProfileVersion {
		tools = []string{"read_file", "list_dir", "search_text", "code_query"}
	}
	model := map[string]any{"provider": "openai-compatible", "baseUrl": "http://127.0.0.1:8091/v1", "apiKey": "offline-mailbox-placeholder", "model": p.Model, "temperature": 0.2, "maxTokens": p.MaxTokens, "stream": false}
	if p.CallSeconds != 0 {
		// SCC waits past the mailbox window so the bounded broker decides.
		model["timeout"] = sccWaitMillis(p)
	}
	config := map[string]any{
		"model":       model,
		"permissions": map[string]any{"autoApprove": tools, "denyPaths": []string{".env", ".env.*", "**/*.key", "**/*.pem", "**/id_rsa*", "**/.ssh/*", "**/credentials*"}},
	}
	if p.Version != ReviewProfileVersion {
		config["run_shell"] = map[string]any{"allowedEnvVars": []string{noBytecodeName}}
	}
	if p.Version == ReviewProfileVersion {
		config["roles"] = map[string]string{"reviewer": p.Model}
		permissions := config["permissions"].(map[string]any)
		permissions["denyCommands"] = []string{"*"}
		permissions["denyGitMutation"] = true
		permissions["profile"] = "traditional"
	}
	raw, _ := json.Marshal(config)
	return raw
}

// sccWaitMillis outlasts the mailbox window, so SCC never abandons (and
// retries) a call that the broker is still allowed to complete.
func sccWaitMillis(p Profile) int64 {
	return (p.CallWindow() + 15*time.Second).Milliseconds()
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
	p, err := parseWorkloadProfile(profileRaw, a.ProfileSHA256)
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
	stateDirectory := WorkspaceDirectory
	if p.Version == ReviewProfileVersion {
		stateDirectory = "/review-state"
	}
	for _, name := range []string{"home", "tmp", "run"} {
		if os.Mkdir(filepath.Join(stateDirectory, name), 0700) != nil {
			return 1
		}
	}
	baselineCtx, stopBaseline := context.WithTimeout(ctx, 20*time.Second)
	if p.Version != ReviewProfileVersion {
		err = InitializeBaseline(baselineCtx, WorkspaceDirectory, "/opt/ai-sdlc/empty-git-template")
	}
	stopBaseline()
	if err != nil {
		return 1
	}
	prompt := stateDirectory + "/run/prompt.md"
	promptText := BuildPrompt(a, policy)
	if p.Version == ReviewProfileVersion {
		promptText = BuildReviewPrompt(a, policy)
	}
	if os.WriteFile(prompt, []byte(promptText), 0600) != nil {
		return 1
	}
	if a.Repair != nil {
		if p.Version != ProfileVersion || p.MaxRepairs != 1 {
			return 1
		}
		// Tool steps and model calls are independent. The durable derived ledger
		// enforces RemainingCalls; do not reduce the original SCC tool ceiling.
		remaining := int(time.Until(time.UnixMilli(a.ExpiresAt)).Seconds())
		if remaining < 1 {
			return 1
		}
		p.MaxSeconds = min(p.MaxSeconds, remaining)
	}
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + stateDirectory + "/home", "TMPDIR=" + stateDirectory + "/tmp", "LC_ALL=C.UTF-8", "NO_COLOR=1", "CI=1",
		"SC_CONFIG_PATH=" + InputDirectory + "/scc-config.json", "SC_BASE_URL=http://127.0.0.1:8091/v1", "SC_MODEL=" + p.Model,
		"NODE_OPTIONS=--max-old-space-size=256", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		noBytecodeName + "=" + noBytecodeValue,
	}
	if p.CallSeconds != 0 {
		// The mailbox sends headers only with the completed answer.
		env = append(env, "SC_PROVIDER_CONNECT_TIMEOUT_MS="+strconv.FormatInt(sccWaitMillis(p), 10))
	}
	runCtx, stopRun := context.WithTimeout(ctx, time.Duration(p.MaxSeconds)*time.Second)
	defer stopRun()
	args := sccArguments(p, prompt, stateDirectory)
	if p.Version == ReviewProfileVersion {
		env = append(env, "SC_ROLE_MAX_FIXES=0")
	}
	err = runBounded(runCtx, "/usr/local/bin/scc", WorkspaceDirectory+"/repository", env, args)
	if err == nil {
		if p.Version == ReviewProfileVersion && VerifySource(WorkspaceDirectory, a) != nil {
			return 1
		}
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if p.Version == ReviewProfileVersion && exit.ExitCode() == 10 && VerifySource(WorkspaceDirectory, a) != nil {
			return 1
		}
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

func sccArguments(p Profile, prompt, stateDirectory string) []string {
	args := []string{"chat", "-yq", "--prompt-file", prompt, "--no-commit", "--audit-log", stateDirectory + "/run/audit.jsonl", "--summary-file", stateDirectory + "/run/summary.json", "--max-seconds", strconv.Itoa(p.MaxSeconds), "--max-steps", strconv.Itoa(p.MaxSteps)}
	if p.Version == ReviewProfileVersion {
		args[1] = "-q"
		args = append(args, "--role", "reviewer")
	}
	return args
}
