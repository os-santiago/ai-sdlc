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
//   - the next container may first be reported created but not started
//     (waiting with its container ID and no reason, exactly as the kubelet
//     writes ContainerStateCreated); it then never returns to that state
//     once observed running or terminated;
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
	created   map[string]bool // created but not yet started
}

// observeInit checks init statuses against the compiled init containers.
// With requireComplete, every init container must have exited 0.
func observeInit(expected []core.Container, statuses []core.ContainerStatus, requireComplete bool) (initProgress, error) {
	progress := initProgress{instances: map[string]instance{}, created: map[string]bool{}}
	if len(expected) == 0 {
		if len(statuses) != 0 {
			return progress, initRefusal("count")
		}
		progress.complete = true
		return progress, nil
	}
	if len(statuses) == 0 {
		if requireComplete {
			return progress, initRefusal("count")
		}
		return progress, nil
	}
	if len(statuses) != len(expected) {
		return progress, initRefusal("count")
	}
	done := true // every earlier init container exited 0
	for i, status := range statuses {
		pin := expected[i].Image
		if status.Name != expected[i].Name || status.RestartCount != 0 || status.LastTerminationState.Terminated != nil ||
			status.LastTerminationState.Running != nil || status.LastTerminationState.Waiting != nil ||
			len(status.Image) > 1024 || len(status.ImageID) > 1024 {
			return progress, initRefusal("identity")
		}
		if status.ImageID != "" && (!imageIDPattern.MatchString(status.ImageID) || !imageMatchesPin(status.Image, status.ImageID, pin)) {
			return progress, initRefusal("image")
		}
		state := status.State
		switch {
		case state.Terminated != nil && state.Running == nil && state.Waiting == nil:
			if !done || !containerIDPattern.MatchString(status.ContainerID) || status.ImageID == "" || status.Image == "" ||
				(state.Terminated.ContainerID != "" && state.Terminated.ContainerID != status.ContainerID) {
				return progress, initRefusal("terminated")
			}
			if state.Terminated.ExitCode != 0 || state.Terminated.Signal != 0 {
				return progress, initFailure(status.Name, state.Terminated)
			}
		case state.Running != nil && state.Terminated == nil && state.Waiting == nil:
			if requireComplete || !done || !containerIDPattern.MatchString(status.ContainerID) || status.ImageID == "" || status.Image == "" {
				return progress, initRefusal("running")
			}
			done = false
		case state.Waiting != nil && state.Running == nil && state.Terminated == nil && status.ContainerID != "":
			// Created, not yet started: the kubelet reports a container the
			// runtime has created but not started as waiting with its ID and
			// no reason. Only the next container in the sequence may be
			// created, and its ID is then held to every later observation.
			if requireComplete || !done || !createdStatus(status) {
				return progress, initRefusal("created")
			}
			progress.created[status.Name] = true
			done = false
		case state.Waiting != nil && state.Running == nil && state.Terminated == nil:
			if requireComplete || (state.Waiting.Reason != "PodInitializing" && state.Waiting.Reason != "ContainerCreating") {
				return progress, initRefusal("waiting")
			}
			done = false
			continue
		default:
			return progress, initRefusal("state")
		}
		progress.instances[status.Name] = instance{status.ContainerID, status.ImageID, status.Image}
	}
	progress.complete = done
	return progress, nil
}

// createdStatus reports the exact status the kubelet writes for a container
// the runtime has created but not yet started (kubelet
// convertToAPIContainerStatuses, ContainerStateCreated): waiting with no
// reason or message, a well-formed container ID, never restarted and no
// previous termination. Its image ID, when already reported, is checked
// against the pin by the caller; a started container must report one.
func createdStatus(status core.ContainerStatus) bool {
	w := status.State.Waiting
	return w != nil && status.State.Running == nil && status.State.Terminated == nil && w.Reason == "" && w.Message == "" &&
		containerIDPattern.MatchString(status.ContainerID) && status.RestartCount == 0 && status.LastTerminationState == (core.ContainerState{})
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

// neverUnstarted refuses a container observed created (not started) after
// it was observed running or terminated: the kubelet never reports that
// regression. started records every container observed running or
// terminated so far; created lists those created but not started now.
func neverUnstarted(started map[string]bool, current map[string]instance, created map[string]bool) bool {
	for name := range created {
		if started[name] {
			return false
		}
	}
	for name := range current {
		if !created[name] {
			started[name] = true
		}
	}
	return true
}

// Exit codes of repository-tests copy-toolchain
// (components/model-mailbox/tests-cmd/toolchain_linux.go).
var copyRefusals = map[int32]string{2: "usage", 3: "source", 4: "link", 5: "limit", 6: "dynamic", 7: "destination"}

// readinessError names why a toolchain init container stopped the pod. It
// unwraps to ErrTransport, so every caller still stops the same way.
// Steps other than toolchain-* name the readiness check that refused the pod
// (see readinessSteps); cause, when set, is the original transport or
// permission error it unwraps to instead.
type readinessError struct {
	step  string
	cause error
}

func (e readinessError) Error() string { return "executor pod stopped at " + e.step }
func (e readinessError) Unwrap() error {
	if e.cause != nil {
		return e.cause
	}
	return ErrTransport
}

func initRefusal(rule string) error { return readinessError{step: "init-" + rule} }

func initFailure(name string, terminated *core.ContainerStateTerminated) error {
	switch {
	case name != executor.InitInstallRunner && name != executor.InitCopyToolchain:
		return ErrTransport
	case terminated.Reason == "OOMKilled":
		return readinessError{step: "toolchain-memory"}
	case name == executor.InitInstallRunner:
		return readinessError{step: "toolchain-install"}
	}
	if reason, ok := copyRefusals[terminated.ExitCode]; ok && terminated.Signal == 0 {
		return readinessError{step: "toolchain-" + reason}
	}
	return readinessError{step: "toolchain-failed"}
}

var readinessSteps = regexp.MustCompile(`^(toolchain-(memory|install|usage|source|link|limit|dynamic|destination|failed)|init-(count|identity|image|terminated|running|created|waiting|state|unstable)|uid|request|permission|deadline|status|template|containers|containers-before-init|containers-created|containers-unstable|phase|bind|bind-unstable)$`)

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
