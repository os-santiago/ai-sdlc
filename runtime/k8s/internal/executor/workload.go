package executor

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

var namespacePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var imagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9./:_-]*@sha256:[a-f0-9]{64}$`)
var runtimeClassPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// DefaultRuntimeClass is the sandbox RuntimeClass the local kind installer
// provides. Other platforms configure their standard sandbox class instead
// (for example gvisor or kata); there is no unsandboxed alternative.
const DefaultRuntimeClass = "ai-sdlc-offline"

// ValidRuntimeClass accepts a Kubernetes RuntimeClass name (DNS-1123
// subdomain). The name is installation configuration, never request or model
// content; an empty name is rejected rather than meaning "no sandbox".
func ValidRuntimeClass(name string) bool {
	return len(name) <= 253 && runtimeClassPattern.MatchString(name)
}

// Workload is a pure compiler output. No API client, admission decision, RBAC or
// provider authority is created here. A trusted controller must bind this exact
// template, resulting Pod UID and immutable inputs to its durable assignment.
type Workload struct {
	ConfigMap map[string]any `json:"configMap"`
	Pod       map[string]any `json:"pod"`
}

// ValidatePlacement rejects mutable image references and malformed scope.
func ValidatePlacement(namespace, workerImage, mailboxImage string) error {
	if !namespacePattern.MatchString(namespace) || len(workerImage) > 512 || len(mailboxImage) > 512 || !imagePattern.MatchString(workerImage) || !imagePattern.MatchString(mailboxImage) {
		return ErrInvalid
	}
	return nil
}

// Compile renders the executor workload in the installation's sandbox
// RuntimeClass; the trusted controller supplies runtimeClass from its loaded
// installation configuration.
func Compile(namespace, workerImage, mailboxImage, runtimeClass string, assignmentRaw, profileRaw, adevRaw []byte, now time.Time) (Workload, error) {
	if ValidatePlacement(namespace, workerImage, mailboxImage) != nil || !ValidRuntimeClass(runtimeClass) {
		return Workload{}, ErrInvalid
	}
	digest := workspacebundle.Digest(assignmentRaw)
	a, err := ParseAssignment(assignmentRaw, digest, now)
	if err != nil {
		return Workload{}, err
	}
	p, err := parseWorkloadProfile(profileRaw, a.ProfileSHA256)
	if err != nil {
		return Workload{}, err
	}
	if a.Repair != nil && (p.Version != ProfileVersion || p.MaxRepairs != 1) {
		return Workload{}, ErrInvalid
	}
	if len(adevRaw) == 0 || len(adevRaw) > 64*1024 || workspacebundle.Digest(adevRaw) != p.ADevSHA256 {
		return Workload{}, ErrInvalid
	}
	name := fmt.Sprintf("executor-%s-%d", a.WorkItemID, a.Attempt)
	configName := "assignment-" + digest[:32]
	labels := map[string]any{"app.kubernetes.io/part-of": "ai-sdlc", "ai-sdlc.io/component": "executor", "ai-sdlc.io/work-item": a.WorkItemID}
	metadata := func(n string) map[string]any {
		return map[string]any{"name": n, "namespace": namespace, "labels": labels, "annotations": map[string]any{"ai-sdlc.io/assignment-sha256": digest, "ai-sdlc.io/authority-revision": a.AuthorityRevision}}
	}
	config := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": metadata(configName), "immutable": true, "data": map[string]any{
		"assignment.json": string(assignmentRaw), "profile.json": string(profileRaw), "adev-profile.yaml": string(adevRaw), "scc-config.json": string(SCCConfiguration(p)),
	}}
	security := map[string]any{"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000, "readOnlyRootFilesystem": true, "allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}}
	workspace := map[string]any{"name": "workspace", "mountPath": "/workspaces"}
	bridge := map[string]any{"name": "bridge", "mountPath": "/bridge"}
	identity := map[string]any{"name": "pod-identity", "mountPath": "/var/run/ai-sdlc/pod", "readOnly": true}
	assignment := map[string]any{"name": "assignment", "mountPath": InputDirectory, "readOnly": true}
	worker := map[string]any{"name": "worker", "image": workerImage, "imagePullPolicy": "IfNotPresent", "command": []string{"/executor", "--assignment-sha256", digest}, "securityContext": security,
		"resources":    map[string]any{"requests": map[string]any{"cpu": "100m", "memory": "128Mi"}, "limits": map[string]any{"cpu": "1", "memory": "768Mi", "ephemeral-storage": "128Mi"}},
		"volumeMounts": []any{workspace, assignment, identity},
	}
	mailbox := map[string]any{"name": "mailbox", "image": mailboxImage, "imagePullPolicy": "IfNotPresent", "command": []string{"/model-mailbox", "serve", "--attempt", fmt.Sprintf("%s-%d", a.WorkItemID, a.Attempt), "--pod-uid", "$(POD_UID)", "--model", p.Model, "--max-tokens", fmt.Sprint(p.MaxTokens)}, "securityContext": security,
		"env":          []any{map[string]any{"name": "POD_UID", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.uid"}}}},
		"resources":    map[string]any{"requests": map[string]any{"cpu": "20m", "memory": "32Mi"}, "limits": map[string]any{"cpu": "500m", "memory": "256Mi", "ephemeral-storage": "128Mi"}},
		"volumeMounts": []any{workspace, bridge, assignment, identity},
	}
	if p.CallSeconds != 0 {
		mailbox["command"] = append(mailbox["command"].([]string), "--wait-seconds", fmt.Sprint(p.CallSeconds))
	}
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": metadata(name), "spec": map[string]any{
		"runtimeClassName": runtimeClass, "automountServiceAccountToken": false, "enableServiceLinks": false, "restartPolicy": "Never", "activeDeadlineSeconds": PodDeadlineSeconds(p), "terminationGracePeriodSeconds": 1,
		"hostNetwork": false, "hostPID": false, "hostIPC": false, "shareProcessNamespace": false,
		"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000, "fsGroup": 1000, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"volumes": []any{
			map[string]any{"name": "workspace", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "64Mi"}},
			map[string]any{"name": "bridge", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "4Mi"}},
			map[string]any{"name": "assignment", "configMap": map[string]any{"name": configName, "defaultMode": 0444}},
			map[string]any{"name": "pod-identity", "downwardAPI": map[string]any{"items": []any{map[string]any{"path": "uid", "fieldRef": map[string]any{"fieldPath": "metadata.uid"}}}}},
		}, "containers": []any{worker, mailbox},
	}}
	if p.Version == ReviewProfileVersion {
		// The reviewer cannot mutate source even if its tool policy fails.
		worker["volumeMounts"] = []any{
			map[string]any{"name": "workspace", "mountPath": "/workspaces", "readOnly": true},
			map[string]any{"name": "review-state", "mountPath": "/review-state"}, assignment, identity,
		}
		mailbox["volumeMounts"] = []any{workspace, bridge, assignment, identity,
			map[string]any{"name": "review-state", "mountPath": "/review-state", "readOnly": true},
		}
		spec := pod["spec"].(map[string]any)
		spec["volumes"] = append(spec["volumes"].([]any), map[string]any{"name": "review-state", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "16Mi"}})
	}
	// JSON round-trip also gives independent objects; callers cannot accidentally
	// mutate shared nested maps through a sibling container/config-map object.
	raw, err := json.Marshal(Workload{config, pod})
	if err != nil {
		return Workload{}, ErrInvalid
	}
	var result Workload
	if json.Unmarshal(raw, &result) != nil {
		return Workload{}, ErrInvalid
	}
	return result, nil
}
