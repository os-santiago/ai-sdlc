package podtransport

import (
	"errors"
	"regexp"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	core "k8s.io/api/core/v1"
)

// An executor pod with a generation toolchain runs the compiled init
// containers (install-runner, then copy-toolchain) before its worker and
// mailbox. The exact pod checks accept exactly that sequence and nothing
// else:
//   - the statuses name the compiled init containers in their compiled order
//     (none at all only before the kubelet first reports);
//   - each runs its pinned image, has never restarted and has no previous
//     termination;
//   - a container starts only after every earlier one exited 0, and later
//     ones wait (PodInitializing or ContainerCreating, with no container ID);
//   - a container ID, once observed, never changes, and the container stays
//     in later observations (an empty list is valid only before the first);
//   - a running pod has every init container terminated with exit code 0,
//     no signal, and the same container ID in its state.
//
// Any other observation (an extra, missing, reordered, restarted, failed or
// substituted init container) refuses the pod, as before.

// initProgress is one valid observation of the init sequence.
type initProgress struct {
	complete  bool
	instances map[string]instance
}

// observeInit checks init statuses against the compiled init containers.
// With requireComplete, every init container must have exited 0.
func observeInit(expected []core.Container, statuses []core.ContainerStatus, requireComplete bool) (initProgress, error) {
	progress := initProgress{instances: map[string]instance{}}
	if len(expected) == 0 {
		if len(statuses) != 0 {
			return progress, ErrTransport
		}
		progress.complete = true
		return progress, nil
	}
	if len(statuses) == 0 {
		if requireComplete {
			return progress, ErrTransport
		}
		return progress, nil
	}
	if len(statuses) != len(expected) {
		return progress, ErrTransport
	}
	done := true // every earlier init container exited 0
	for i, status := range statuses {
		pin := expected[i].Image
		if status.Name != expected[i].Name || status.RestartCount != 0 || status.LastTerminationState.Terminated != nil ||
			status.LastTerminationState.Running != nil || status.LastTerminationState.Waiting != nil ||
			len(status.Image) > 1024 || len(status.ImageID) > 1024 {
			return progress, ErrTransport
		}
		if status.ImageID != "" && (!imageIDPattern.MatchString(status.ImageID) || !imageMatchesPin(status.Image, status.ImageID, pin)) {
			return progress, ErrTransport
		}
		state := status.State
		switch {
		case state.Terminated != nil && state.Running == nil && state.Waiting == nil:
			if !done || !containerIDPattern.MatchString(status.ContainerID) || status.ImageID == "" || status.Image == "" ||
				(state.Terminated.ContainerID != "" && state.Terminated.ContainerID != status.ContainerID) {
				return progress, ErrTransport
			}
			if state.Terminated.ExitCode != 0 || state.Terminated.Signal != 0 {
				return progress, initFailure(status.Name, state.Terminated)
			}
		case state.Running != nil && state.Terminated == nil && state.Waiting == nil:
			if requireComplete || !done || !containerIDPattern.MatchString(status.ContainerID) || status.ImageID == "" || status.Image == "" {
				return progress, ErrTransport
			}
			done = false
		case state.Waiting != nil && state.Running == nil && state.Terminated == nil:
			if requireComplete || status.ContainerID != "" || (state.Waiting.Reason != "PodInitializing" && state.Waiting.Reason != "ContainerCreating") {
				return progress, ErrTransport
			}
			done = false
			continue
		default:
			return progress, ErrTransport
		}
		progress.instances[status.Name] = instance{status.ContainerID, status.ImageID, status.Image}
	}
	progress.complete = done
	return progress, nil
}

// stableInit requires every init container seen earlier to be observed
// again with the same container ID, and records new ones. A container that
// disappears (including an empty status list after the first observation)
// or goes back to waiting refuses the pod.
func stableInit(seen map[string]instance, current map[string]instance) bool {
	for name, previous := range seen {
		if observed, ok := current[name]; !ok || observed.ID != previous.ID {
			return false
		}
	}
	for name, observed := range current {
		seen[name] = observed
	}
	return true
}

// Exit codes of repository-tests copy-toolchain
// (components/model-mailbox/tests-cmd/toolchain_linux.go).
var copyRefusals = map[int32]string{2: "usage", 3: "source", 4: "link", 5: "limit", 6: "dynamic", 7: "destination"}

// readinessError names why a toolchain init container stopped the pod. It
// unwraps to ErrTransport, so every caller still stops the same way.
type readinessError struct{ step string }

func (e readinessError) Error() string { return "executor pod stopped at " + e.step }
func (e readinessError) Unwrap() error { return ErrTransport }

func initFailure(name string, terminated *core.ContainerStateTerminated) error {
	switch {
	case name != executor.InitInstallRunner && name != executor.InitCopyToolchain:
		return ErrTransport
	case terminated.Reason == "OOMKilled":
		return readinessError{"toolchain-memory"}
	case name == executor.InitInstallRunner:
		return readinessError{"toolchain-install"}
	}
	if reason, ok := copyRefusals[terminated.ExitCode]; ok && terminated.Signal == 0 {
		return readinessError{"toolchain-" + reason}
	}
	return readinessError{"toolchain-failed"}
}

var readinessSteps = regexp.MustCompile(`^toolchain-(memory|install|usage|source|link|limit|dynamic|destination|failed)$`)

// ReadinessStep returns the named readiness stop, or "" for any other error.
func ReadinessStep(err error) string {
	var stopped readinessError
	if errors.As(err, &stopped) {
		return stopped.step
	}
	return ""
}

// KnownReadinessStep reports whether step is one ReadinessStep can return.
func KnownReadinessStep(step string) bool { return readinessSteps.MatchString(step) }

// startBudget is StartBudget, plus the toolchain image pull and copy for a
// pod with the compiled init sequence.
func startBudget(pod core.Pod) time.Duration {
	if len(pod.Spec.InitContainers) != 0 {
		return StartBudget + executor.ToolchainStartSeconds*time.Second
	}
	return StartBudget
}
