package repositorytests

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Fixed layout of a repository-tests pod.
const (
	SettingsDirectory = "/etc/ai-sdlc/tests"
	RunnerPath        = "/runner/component"
	ScratchDirectory  = "/scratch"
	// ReadySeconds bounds image start, import and the runner's marker wait.
	ReadySeconds = 120
	// MarginSeconds covers tree verification, the report and termination.
	MarginSeconds = 60
)

var (
	ErrWorkload      = errors.New("invalid repository tests workload")
	namespacePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	imagePattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9./:_-]*@sha256:[a-f0-9]{64}$`)
	uuidPattern      = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
)

// Pod is one run of the three-tree verification.
type Pod struct {
	Namespace     string
	VerifierImage string // provides the static runner, copied by an init container
	MailboxImage  string // provides workspace-transfer for import, ready and hold
	WorkItemID    string
	Attempt       int
	Run           string // RunNames
	SourceSHA     string
	TreeSHA256    string
	Verification  project.Verification
}

// RunNames lists the runs of one verification in order: the baseline, the
// cross run and up to three candidate runs.
func RunNames(repeat int) []string {
	names := []string{"baseline", "cross"}
	for i := 1; i <= repeat; i++ {
		names = append(names, fmt.Sprintf("candidate-%d", i))
	}
	return names
}

// Workload is a pure compiler output: an immutable settings ConfigMap and the
// Pod. A trusted controller creates them and binds the resulting Pod UID.
type Workload struct {
	ConfigMap map[string]any `json:"configMap"`
	Pod       map[string]any `json:"pod"`
}

func (p Pod) validate() error {
	known := false
	for _, name := range RunNames(p.Verification.Repeat) {
		known = known || name == p.Run
	}
	if !namespacePattern.MatchString(p.Namespace) || len(p.VerifierImage) > 512 || len(p.MailboxImage) > 512 ||
		!imagePattern.MatchString(p.VerifierImage) || !imagePattern.MatchString(p.MailboxImage) ||
		!uuidPattern.MatchString(p.WorkItemID) || p.Attempt < 1 || p.Attempt > 16 || !known ||
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

// DeadlineSeconds is the pod's hard deadline for a test timeout.
func DeadlineSeconds(v project.Verification) int {
	return ReadySeconds + v.TimeoutSeconds + MarginSeconds
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
	name := fmt.Sprintf("tests-%s-%d-%s", p.WorkItemID, p.Attempt, p.Run)
	configName := "repository-tests-" + settingsSHA[:32]
	labels := map[string]any{"app.kubernetes.io/part-of": "ai-sdlc", "ai-sdlc.io/component": "repository-tests", "ai-sdlc.io/work-item": p.WorkItemID, "ai-sdlc.io/run": p.Run}
	metadata := func(n string) map[string]any {
		return map[string]any{"name": n, "namespace": p.Namespace, "labels": labels, "annotations": map[string]any{
			"ai-sdlc.io/settings-sha256": settingsSHA, "ai-sdlc.io/source-sha": p.SourceSHA, "ai-sdlc.io/tree-sha256": p.TreeSHA256}}
	}
	config := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": metadata(configName), "immutable": true,
		"data": map[string]any{"verification.json": string(settings)}}
	security := map[string]any{"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000, "readOnlyRootFilesystem": true, "allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}}
	workspace := map[string]any{"name": "workspace", "mountPath": "/workspaces"}
	identity := map[string]any{"name": "pod-identity", "mountPath": "/var/run/ai-sdlc/pod", "readOnly": true}
	small := map[string]any{"requests": map[string]any{"cpu": "20m", "memory": "32Mi"}, "limits": map[string]any{"cpu": "200m", "memory": "64Mi", "ephemeral-storage": "64Mi"}}
	// The runner volume is disk-backed: a memory-backed copy would be charged
	// to the init container's small memory limit.
	copying := map[string]any{"requests": map[string]any{"cpu": "20m", "memory": "32Mi"}, "limits": map[string]any{"cpu": "200m", "memory": "64Mi", "ephemeral-storage": "128Mi"}}
	install := map[string]any{"name": "install-runner", "image": p.VerifierImage, "imagePullPolicy": "IfNotPresent", "command": []string{"/component", "install-runner", RunnerPath},
		"securityContext": security, "resources": copying, "volumeMounts": []any{map[string]any{"name": "runner", "mountPath": "/runner"}}}
	transfer := map[string]any{"name": "transfer", "image": p.MailboxImage, "imagePullPolicy": "IfNotPresent", "command": []string{"/workspace-transfer", "hold", "--pod-uid", "$(POD_UID)"},
		"env":             []any{map[string]any{"name": "POD_UID", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.uid"}}}},
		"securityContext": security, "resources": small, "volumeMounts": []any{workspace, identity}}
	quota := map[string]any{"cpu": p.Verification.CPU, "memory": p.Verification.Memory, "ephemeral-storage": p.Verification.EphemeralStorage}
	tests := map[string]any{"name": "tests", "image": p.Verification.Image, "imagePullPolicy": "IfNotPresent",
		"command":         []string{RunnerPath, "repository-tests-run", SettingsDirectory + "/verification.json", ScratchDirectory, p.SourceSHA, p.TreeSHA256},
		"securityContext": security, "resources": map[string]any{"requests": quota, "limits": quota},
		"volumeMounts": []any{workspace, identity,
			map[string]any{"name": "scratch", "mountPath": ScratchDirectory},
			map[string]any{"name": "runner", "mountPath": "/runner", "readOnly": true},
			map[string]any{"name": "settings", "mountPath": SettingsDirectory, "readOnly": true}}}
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": metadata(name), "spec": map[string]any{
		"runtimeClassName": "ai-sdlc-offline", "automountServiceAccountToken": false, "enableServiceLinks": false, "restartPolicy": "Never",
		"activeDeadlineSeconds": DeadlineSeconds(p.Verification), "terminationGracePeriodSeconds": 1,
		"hostNetwork": false, "hostPID": false, "hostIPC": false, "shareProcessNamespace": false,
		"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000, "fsGroup": 1000, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"volumes": []any{
			// The imported tree is bounded by the bundle limits; the rest is
			// room for build output the tests write next to the sources.
			map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": "256Mi"}},
			map[string]any{"name": "scratch", "emptyDir": map[string]any{"sizeLimit": p.Verification.EphemeralStorage}},
			map[string]any{"name": "runner", "emptyDir": map[string]any{"sizeLimit": "128Mi"}},
			map[string]any{"name": "settings", "configMap": map[string]any{"name": configName, "defaultMode": 0o444}},
			map[string]any{"name": "pod-identity", "downwardAPI": map[string]any{"items": []any{map[string]any{"path": "uid", "fieldRef": map[string]any{"fieldPath": "metadata.uid"}}}}},
		},
		"initContainers": []any{install},
		"containers":     []any{transfer, tests},
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
