package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// ToolServerName names the generator tools MCP server in SCC's configuration.
const ToolServerName = "ai-sdlc"

// SCCConfiguration is deterministic and contains no provider credential. The
// endpoint is Pod-local; external model calls require a separate authority.
// noBytecode keeps repository commands (for example a Python test run) from
// writing __pycache__ files into the workspace; the candidate delta is the
// workspace, and recipes accept only the intended source changes. SCC hands
// run_shell children an allowlisted environment, so the name is allowlisted
// in its configuration as well.
const noBytecodeName, noBytecodeValue = "PYTHONDONTWRITEBYTECODE", "1"

func SCCConfiguration(p Profile, toolchain *Toolchain) []byte {
	tools := []string{"read_file", "list_dir", "search_text", "memory_read", "write_file", "edit_file", "run_shell", "git", "memory_write", "code_query", "repo_probe"}
	if p.Version == ReviewProfileVersion {
		tools = []string{"read_file", "list_dir", "search_text", "code_query"}
	}
	model := map[string]any{"provider": "openai-compatible", "baseUrl": "http://127.0.0.1:8091/v1", "apiKey": "offline-mailbox-placeholder", "model": p.Model, "temperature": 0.2, "maxTokens": p.MaxTokens, "stream": false}
	if p.CallSeconds != 0 {
		// SCC waits past the mailbox window so the bounded broker decides.
		model["timeout"] = sccWaitMillis(p)
	}
	if p.Version == ProfileVersion && p.Tools != nil {
		// SCC loads MCP servers only from this trusted configuration; tools
		// surface as mcp__<server>__<tool> and need the same auto-approval.
		for _, tool := range p.Tools.Spec.Tools {
			tools = append(tools, "mcp__"+ToolServerName+"__"+tool.Name)
		}
	}
	config := map[string]any{
		"model":       model,
		"permissions": map[string]any{"autoApprove": tools, "denyPaths": []string{".env", ".env.*", "**/*.key", "**/*.pem", "**/id_rsa*", "**/.ssh/*", "**/credentials*"}},
	}
	if p.Version == ProfileVersion && p.Tools != nil {
		spec, _ := json.Marshal(p.Tools.Spec)
		config["mcp"] = map[string]any{"servers": map[string]any{ToolServerName: map[string]any{
			"command":   "/executor",
			"args":      []string{"tools-mcp", "--spec", string(spec), "--wait-seconds", fmt.Sprint(p.Tools.Seconds)},
			"timeoutMs": (p.Tools.Seconds + 30) * 1000,
		}}}
	}
	if p.Version != ReviewProfileVersion {
		// Toolchain variables reach run_shell children the same way; PATH,
		// TMPDIR and XDG_CACHE_HOME are already in SCC's base allowlist.
		config["run_shell"] = map[string]any{"allowedEnvVars": append([]string{noBytecodeName}, toolchain.envNames()...)}
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

// ProjectedProcessSeconds reads the projected assignment and profile, bound to
// assignmentDigest, and returns their ProcessSeconds. Any invalid input keeps
// DefaultProcessSeconds; Run then rejects the same input.
func ProjectedProcessSeconds(inputDirectory, assignmentDigest string, now time.Time) int {
	raw, err := readProjected(inputDirectory+"/assignment.json", MaxAssignmentBytes)
	if err != nil {
		return DefaultProcessSeconds
	}
	a, err := ParseAssignment(raw, assignmentDigest, now)
	if err != nil {
		return DefaultProcessSeconds
	}
	profileRaw, err := readProjected(inputDirectory+"/profile.json", MaxProfileBytes)
	if err != nil {
		return DefaultProcessSeconds
	}
	p, err := parseWorkloadProfile(profileRaw, a.ProfileSHA256)
	if err != nil {
		return DefaultProcessSeconds
	}
	return ProcessSeconds(p)
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
	if err != nil || string(config) != string(SCCConfiguration(p, a.Toolchain)) {
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
		promptText = BuildReviewPrompt(a, policy, p.MaxSteps)
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
	if a.Toolchain != nil && prepareToolchain(ToolchainDirectory, ScratchDirectory, a.Toolchain) != nil {
		return 1
	}
	env := launchEnvironment(p, a.Toolchain, stateDirectory)
	runCtx, stopRun := context.WithTimeout(ctx, time.Duration(p.MaxSeconds)*time.Second)
	defer stopRun()
	args := sccArguments(p, prompt, stateDirectory)
	if p.Version == ReviewProfileVersion {
		env = append(env, "SC_ROLE_MAX_FIXES=0")
	}
	started := time.Now()
	err = runBounded(runCtx, "/usr/local/bin/scc", WorkspaceDirectory+"/repository", env, args)
	// SCC measures its time budget on the wall clock; the monotonic reading
	// stops during a host suspend, and the wall clock can be stepped. Both
	// are kept and the classification uses the larger.
	monotonic, wall := time.Since(started), time.Now().Round(0).Sub(started.Round(0))
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
		case 22:
			return budgetExitCode(p, monotonic, wall)
		case 10, 20, 21, 23:
			return exit.ExitCode()
		}
	}
	if runCtx.Err() != nil {
		return 124
	}
	return 1
}

// launchEnvironment is SCC's complete, explicit environment. With a
// toolchain, its declared directories precede the worker image's on PATH,
// TMPDIR and the XDG cache move to the disk-backed scratch volume, and the
// reviewed toolchain variables follow in a fixed order.
func launchEnvironment(p Profile, toolchain *Toolchain, stateDirectory string) []string {
	temporary := stateDirectory + "/tmp"
	if toolchain != nil {
		temporary = ScratchDirectory + "/tmp"
	}
	env := []string{
		"PATH=" + toolchain.SearchPath(), "HOME=" + stateDirectory + "/home", "TMPDIR=" + temporary, "LC_ALL=C.UTF-8", "NO_COLOR=1", "CI=1",
		"SC_CONFIG_PATH=" + InputDirectory + "/scc-config.json", "SC_BASE_URL=http://127.0.0.1:8091/v1", "SC_MODEL=" + p.Model,
		"NODE_OPTIONS=--max-old-space-size=256", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		noBytecodeName + "=" + noBytecodeValue,
	}
	if p.CallSeconds != 0 {
		// The mailbox sends headers only with the completed answer.
		env = append(env, "SC_PROVIDER_CONNECT_TIMEOUT_MS="+strconv.FormatInt(sccWaitMillis(p), 10))
	}
	if toolchain != nil {
		env = append(env, "XDG_CACHE_HOME="+ScratchDirectory+"/cache")
		for _, name := range toolchain.envNames() {
			env = append(env, name+"="+toolchain.Env[name])
		}
	}
	return env
}

// shadowedTools are the worker programs SCC, its tools and their shells
// resolve through PATH (SCC runs git and probes for bwrap by name; scc starts
// with "env node"). The toolchain directories come first on PATH, so a
// toolchain that ships one of them would replace the worker's own; such a
// run stops.
var shadowedTools = []string{"node", "git", "scc", "sh", "bash", "env", "bwrap", "busybox", "sed", "python3"}

// prepareToolchain checks the copied toolchain and creates the scratch
// directories. The copy was made by the init container; it is read-only.
func prepareToolchain(toolchainDirectory, scratchDirectory string, t *Toolchain) error {
	info, err := os.Lstat(toolchainDirectory)
	if err != nil || !info.IsDir() {
		return ErrInvalid
	}
	for _, dir := range t.Path {
		for _, tool := range shadowedTools {
			if _, err := os.Lstat(filepath.Join(toolchainDirectory, dir, tool)); !errors.Is(err, os.ErrNotExist) {
				return ErrInvalid
			}
		}
	}
	for _, name := range []string{"tmp", "cache"} {
		if os.Mkdir(filepath.Join(scratchDirectory, name), 0700) != nil {
			return ErrInvalid
		}
	}
	return nil
}

func sccArguments(p Profile, prompt, stateDirectory string) []string {
	args := []string{"chat", "-yq", "--prompt-file", prompt, "--no-commit", "--audit-log", stateDirectory + "/run/audit.jsonl", "--summary-file", stateDirectory + "/run/summary.json", "--max-seconds", strconv.Itoa(SCCSeconds(p)), "--max-steps", strconv.Itoa(p.MaxSteps)}
	if p.Version == ReviewProfileVersion {
		args[1] = "-q"
		args = append(args, "--role", "reviewer")
	}
	return args
}
