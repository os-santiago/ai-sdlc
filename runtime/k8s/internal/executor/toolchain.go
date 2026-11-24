package executor

import (
	"path"
	"sort"
	"strings"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

// Toolchain is installation-controlled assignment context, never issue or
// model text: the repository's self-contained toolchain from its reviewed
// project manifest. Image is verification.image, the pinned image the
// verifier runs the same tests in; Root and Path come from
// generation.toolchain; Command and WorkingDir are the verification command;
// Env is verification.env without PATH and without the names the launcher
// reserves. An assignment without it compiles the original workload.
type Toolchain struct {
	Image      string            `json:"image"`
	Root       string            `json:"root"`
	Path       []string          `json:"path"`
	Command    []string          `json:"command"`
	WorkingDir string            `json:"workingDir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// Fixed layout and bounds of a toolchain workload.
const (
	// ToolchainMount is where the worker mounts the toolchain volume
	// read-only; the copy is the directory ToolchainDirectory inside it.
	ToolchainMount     = "/toolchain"
	ToolchainDirectory = ToolchainMount + "/root"
	// ScratchDirectory is the worker's disk-backed scratch volume: TMPDIR
	// and the build cache, outside the memory-backed workspace.
	ScratchDirectory = "/scratch"
	// ToolchainStartSeconds is the start time a toolchain workload adds to
	// the Pending phase and to every deadline that covers it: pulling the
	// pinned toolchain image and the bounded copy.
	ToolchainStartSeconds = 180

	// copyMount is where the copy container mounts the toolchain volume.
	// A toolchain root can never be under /workspaces or /runner, so neither
	// mount can shadow the root being copied.
	copyMount        = "/workspaces"
	copyDestination  = copyMount + "/root"
	runnerMount      = "/runner"
	runnerPath       = runnerMount + "/repository-tests"
	toolchainVolume  = "1Gi"
	scratchVolume    = "1Gi"
	runnerVolume     = "128Mi"
	toolchainMemory  = "1280Mi" // the original 768Mi worker limit plus 512Mi
	toolchainStorage = "2304Mi" // 128Mi + toolchain + scratch + runner volumes
	baseSearchPath   = "/usr/local/bin:/usr/bin:/bin"
)

// Names of the init containers of a toolchain workload, in order.
const (
	InitInstallRunner = "install-runner"
	InitCopyToolchain = "copy-toolchain"
)

// launcherEnv are variables the launcher sets for SCC itself; a toolchain
// variable can never replace one. Names with these prefixes configure SCC,
// Node.js, Git, npm or the XDG directories the launcher owns.
var launcherEnv = map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LC_ALL": true, "NO_COLOR": true, "CI": true, noBytecodeName: true}
var launcherEnvPrefixes = []string{"SC_", "NODE_", "GIT_", "NPM_", "XDG_"}

// ToolchainEnvAllowed reports whether a verification variable may reach the
// generation worker. PATH is rebuilt from the toolchain's declared path.
func ToolchainEnvAllowed(name string) bool {
	if launcherEnv[name] {
		return false
	}
	for _, prefix := range launcherEnvPrefixes {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}

func (t *Toolchain) valid() bool {
	if t == nil || len(t.Image) > 512 || !imagePattern.MatchString(t.Image) || !project.ValidImage(t.Image) ||
		(project.Toolchain{Root: t.Root, Path: t.Path}).Validate() != nil || !project.ValidCommand(t.Command, t.WorkingDir) {
		return false
	}
	if t.Env != nil && (len(t.Env) == 0 || !project.ValidEnv(t.Env)) {
		return false
	}
	for name := range t.Env {
		if !ToolchainEnvAllowed(name) {
			return false
		}
	}
	return true
}

// SearchPath is the worker's PATH: the declared toolchain directories, in
// order, before the worker image's own directories.
func (t *Toolchain) SearchPath() string {
	if t == nil {
		return baseSearchPath
	}
	dirs := make([]string, 0, len(t.Path)+1)
	for _, dir := range t.Path {
		dirs = append(dirs, path.Join(ToolchainDirectory, dir))
	}
	return strings.Join(append(dirs, baseSearchPath), ":")
}

// envNames are the toolchain variables in a fixed order.
func (t *Toolchain) envNames() []string {
	if t == nil {
		return nil
	}
	names := make([]string, 0, len(t.Env))
	for name := range t.Env {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// toolchainInitContainers copies the reviewed root: the mailbox image installs
// the static copier, then the pinned toolchain image runs it. Both run with
// the same restricted security context as the worker, and a refusal fails
// the restartPolicy Never pod before any worker starts.
func toolchainInitContainers(t *Toolchain, mailboxImage string, security map[string]any) []any {
	install := map[string]any{"name": InitInstallRunner, "image": mailboxImage, "imagePullPolicy": "IfNotPresent", "command": []string{"/repository-tests", "install", runnerPath},
		"securityContext": security,
		"resources":       map[string]any{"requests": map[string]any{"cpu": "20m", "memory": "32Mi"}, "limits": map[string]any{"cpu": "200m", "memory": "64Mi", "ephemeral-storage": "128Mi"}},
		"volumeMounts":    []any{map[string]any{"name": "runner", "mountPath": runnerMount}},
	}
	copier := map[string]any{"name": InitCopyToolchain, "image": t.Image, "imagePullPolicy": "IfNotPresent", "command": []string{runnerPath, "copy-toolchain", t.Root, copyDestination},
		"securityContext": security,
		"resources":       map[string]any{"requests": map[string]any{"cpu": "50m", "memory": "32Mi"}, "limits": map[string]any{"cpu": "1", "memory": "512Mi", "ephemeral-storage": "128Mi"}},
		"volumeMounts": []any{map[string]any{"name": "runner", "mountPath": runnerMount, "readOnly": true},
			map[string]any{"name": "toolchain", "mountPath": copyMount}},
	}
	return []any{install, copier}
}

// toolchainVolumes are disk-backed: a memory-backed copy would be charged to
// the containers' memory limits.
func toolchainVolumes() []any {
	return []any{
		map[string]any{"name": "toolchain", "emptyDir": map[string]any{"sizeLimit": toolchainVolume}},
		map[string]any{"name": "scratch", "emptyDir": map[string]any{"sizeLimit": scratchVolume}},
		map[string]any{"name": "runner", "emptyDir": map[string]any{"sizeLimit": runnerVolume}},
	}
}
