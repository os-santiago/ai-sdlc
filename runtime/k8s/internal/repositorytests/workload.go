package repositorytests

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Fixed layout of a repository-tests pod.
const (
	SettingsDirectory = "/etc/ai-sdlc/tests"
	RunnerPath        = "/runner/repository-tests"
	// ScratchDirectory is HOME and TMPDIR; it is /tmp because many tools
	// write there regardless of TMPDIR, and the root filesystem is read-only.
	ScratchDirectory = "/tmp"
	// PullSeconds bounds scheduling, image pulls and the runner install
	// before the runner starts waiting for its marker (ReadySeconds).
	PullSeconds     = 300
	workspaceVolume = "256Mi"
	runnerVolume    = "128Mi"
	// MarginSeconds covers tree verification, the report and termination.
	MarginSeconds = 60
)

var (
	ErrWorkload      = errors.New("invalid repository tests workload")
	namespacePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	imagePattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9./:_-]*@sha256:[a-f0-9]{64}$`)
	uuidPattern      = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
	verificationID   = regexp.MustCompile(`^verification-[a-f0-9]{64}$`)
)

// Runs of one verification. Each is one pod; the candidate pod runs the
// command Repeat times, each time on a fresh copy of the verified tree.
const (
	RunBaseline  = "baseline"
	RunCross     = "cross"
	RunCandidate = "candidate"
)

// RunNames lists the runs in order.
func RunNames() []string { return []string{RunBaseline, RunCross, RunCandidate} }

// LedgerKind is the verification effect kind (and resource name suffix) of a
// run's pod: the cross run executes the candidate's tests on the base code.
func LedgerKind(run string) string {
	switch run {
	case RunBaseline:
		return "baseline"
	case RunCross:
		return "tests"
	case RunCandidate:
		return "candidate"
	}
	return ""
}

// Repetitions is how many times a run executes the command.
func Repetitions(run string, v project.Verification) int {
	if run == RunCandidate {
		return v.Repeat
	}
	return 1
}

// Pod is one run of the three-tree verification. Names derive from the
// signed verification command, so they fit the verifier's effect ledger.
type Pod struct {
	Namespace      string
	MailboxImage   string // provides the static runner (copied by an init container) and workspace-transfer
	RuntimeClass   string // installation sandbox RuntimeClass; required
	VerificationID string // verification-<sha256> of the signed command
	WorkItemID     string
	Run            string // RunNames
	SourceSHA      string
	TreeSHA256     string
	Verification   project.Verification
}

// Names returns the settings ConfigMap (shared by the runs of one
// verification) and the run's pod name.
func (p Pod) Names() (config, pod string) {
	if !verificationID.MatchString(p.VerificationID) || LedgerKind(p.Run) == "" {
		return "", ""
	}
	prefix := "verify-" + p.VerificationID[13:45]
	return prefix + "-inputs", prefix + "-" + LedgerKind(p.Run)
}

// Workload is a pure compiler output: an immutable settings ConfigMap and the
// Pod. A trusted controller creates them and binds the resulting Pod UID.
type Workload struct {
	ConfigMap map[string]any `json:"configMap"`
	Pod       map[string]any `json:"pod"`
}

func (p Pod) validate() error {
	if config, _ := p.Names(); config == "" || !namespacePattern.MatchString(p.Namespace) || len(p.MailboxImage) > 512 ||
		!imagePattern.MatchString(p.MailboxImage) || !executor.ValidRuntimeClass(p.RuntimeClass) ||
		!uuidPattern.MatchString(p.WorkItemID) ||
		!workspacebundle.ValidSourceSHA(p.SourceSHA) || !workspacebundle.ValidDigest(p.TreeSHA256) ||
		p.Verification.Validate() != nil {
		return ErrWorkload
	}
	return nil
}

// Settings is the canonical verification settings document the runner reads.
func Settings(v project.Verification) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if v.Validate() != nil || enc.Encode(v) != nil {
		return nil, ErrWorkload
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// AnalyzeArgument is the runner's optional last argument for a run that also
// executes the manifest's analysis commands.
const AnalyzeArgument = "analyze"

// Analyzes reports whether a run executes the manifest's analysis commands:
// the baseline and the candidate do, so only new findings refuse; the cross
// run (old code, new tests) does not.
func Analyzes(run string, v project.Verification) bool {
	return v.Analysis != nil && (run == RunBaseline || run == RunCandidate)
}

// AnalysisSeconds is the time a run's analysis commands may take, 0 without.
func AnalysisSeconds(run string, v project.Verification) int {
	if !Analyzes(run, v) {
		return 0
	}
	return len(v.Analysis.Commands) * v.Analysis.TimeoutSeconds
}

// DeadlineSeconds is the pod's hard deadline for a test timeout and, with
// analysis, the analysis timeouts, and, with services, the longest service
// readiness wait.
func DeadlineSeconds(run string, v project.Verification) int {
	return PullSeconds + ReadySeconds + v.ServiceReadySeconds() + Repetitions(run, v)*v.TimeoutSeconds + AnalysisSeconds(run, v) + MarginSeconds
}

// CompilePod renders one offline run. The repository's command is never in
// the pod spec: it reaches the runner only through the settings ConfigMap,
// so no Kubernetes variable expansion or argument splitting applies to it.
func CompilePod(p Pod) (Workload, error) {
	if p.validate() != nil {
		return Workload{}, ErrWorkload
	}
	settings, err := Settings(p.Verification)
	if err != nil {
		return Workload{}, ErrWorkload
	}
	settingsSHA := authority.Digest(settings)
	configName, name := p.Names()
	labels := map[string]any{"app.kubernetes.io/part-of": "ai-sdlc", "ai-sdlc.io/component": "repository-tests", "ai-sdlc.io/work-item": p.WorkItemID}
	common := map[string]any{"ai-sdlc.io/command": p.VerificationID, "ai-sdlc.io/settings-sha256": settingsSHA}
	podAnnotations := map[string]any{"ai-sdlc.io/run": p.Run, "ai-sdlc.io/source-sha": p.SourceSHA, "ai-sdlc.io/tree-sha256": p.TreeSHA256}
	for k, v := range common {
		podAnnotations[k] = v
	}
	// The settings are the same for every run of one verification.
	config := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": configName, "namespace": p.Namespace, "labels": labels, "annotations": common}, "immutable": true,
		"data": map[string]any{"verification.json": string(settings)}}
	security := map[string]any{"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000, "readOnlyRootFilesystem": true, "allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}}
	workspace := map[string]any{"name": "workspace", "mountPath": "/workspaces"}
	identity := map[string]any{"name": "pod-identity", "mountPath": "/var/run/ai-sdlc/pod", "readOnly": true}
	small := map[string]any{"requests": map[string]any{"cpu": "20m", "memory": "32Mi"}, "limits": map[string]any{"cpu": "200m", "memory": "64Mi", "ephemeral-storage": "64Mi"}}
	// The runner volume is disk-backed: a memory-backed copy would be charged
	// to the init container's small memory limit.
	copying := map[string]any{"requests": map[string]any{"cpu": "20m", "memory": "32Mi"}, "limits": map[string]any{"cpu": "200m", "memory": "64Mi", "ephemeral-storage": "128Mi"}}
	install := map[string]any{"name": "install-runner", "image": p.MailboxImage, "imagePullPolicy": "IfNotPresent", "command": []string{"/repository-tests", "install", RunnerPath},
		"securityContext": security, "resources": copying, "volumeMounts": []any{map[string]any{"name": "runner", "mountPath": "/runner"}}}
	// transfer is a native sidecar: it starts before the tests container and
	// is stopped when it exits, so the pod completes with the tests.
	transfer := map[string]any{"name": "transfer", "image": p.MailboxImage, "imagePullPolicy": "IfNotPresent", "restartPolicy": "Always", "command": []string{"/workspace-transfer", "hold", "--pod-uid", "$(POD_UID)"},
		"env":             []any{map[string]any{"name": "POD_UID", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.uid"}}}},
		"securityContext": security, "resources": small, "volumeMounts": []any{workspace, identity}}
	// The pod is evicted when its disk use exceeds the sum of its container
	// limits, so the tests container's limit also covers the shared volumes.
	disk := resource.MustParse(p.Verification.EphemeralStorage)
	disk.Add(resource.MustParse(workspaceVolume))
	disk.Add(resource.MustParse(runnerVolume))
	quota := map[string]any{"cpu": p.Verification.CPU, "memory": p.Verification.Memory, "ephemeral-storage": disk.String()}
	command := []string{RunnerPath, "run", SettingsDirectory + "/verification.json", ScratchDirectory, p.SourceSHA, p.TreeSHA256, fmt.Sprint(Repetitions(p.Run, p.Verification))}
	if Analyzes(p.Run, p.Verification) {
		// Only manifests with analysis add the argument, so every other pod
		// renders exactly as before.
		command = append(command, AnalyzeArgument)
	}
	tests := map[string]any{"name": "tests", "image": p.Verification.Image, "imagePullPolicy": "IfNotPresent",
		"command":         command,
		"securityContext": security, "resources": map[string]any{"requests": quota, "limits": quota},
		"volumeMounts": []any{workspace, identity,
			map[string]any{"name": "scratch", "mountPath": ScratchDirectory},
			map[string]any{"name": "runner", "mountPath": "/runner", "readOnly": true},
			map[string]any{"name": "settings", "mountPath": SettingsDirectory, "readOnly": true}}}
	// Services follow the transfer sidecar, so the controller's exec target
	// is up first; only manifests with services add containers or volumes,
	// so every other pod renders exactly as before.
	services, serviceVolumes := serviceSidecars(p.Verification)
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": name, "namespace": p.Namespace, "labels": labels, "annotations": podAnnotations}, "spec": map[string]any{
		"runtimeClassName": p.RuntimeClass, "automountServiceAccountToken": false, "enableServiceLinks": false, "restartPolicy": "Never",
		"activeDeadlineSeconds": DeadlineSeconds(p.Run, p.Verification), "terminationGracePeriodSeconds": 1,
		"hostNetwork": false, "hostPID": false, "hostIPC": false, "shareProcessNamespace": false,
		"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000, "fsGroup": 1000, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"volumes": append([]any{
			// The imported tree is bounded by the bundle limits; the rest is
			// room for build output the tests write next to the sources.
			map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": workspaceVolume}},
			map[string]any{"name": "scratch", "emptyDir": map[string]any{"sizeLimit": p.Verification.EphemeralStorage}},
			map[string]any{"name": "runner", "emptyDir": map[string]any{"sizeLimit": runnerVolume}},
			map[string]any{"name": "settings", "configMap": map[string]any{"name": configName, "defaultMode": 0o444}},
			map[string]any{"name": "pod-identity", "downwardAPI": map[string]any{"items": []any{map[string]any{"path": "uid", "fieldRef": map[string]any{"fieldPath": "metadata.uid"}}}}},
		}, serviceVolumes...),
		"initContainers": append([]any{install, transfer}, services...),
		"containers":     []any{tests},
	}}
	raw, err := json.Marshal(Workload{config, pod})
	if err != nil {
		return Workload{}, ErrWorkload
	}
	var result Workload
	if json.Unmarshal(raw, &result) != nil {
		return Workload{}, ErrWorkload
	}
	return result, nil
}
