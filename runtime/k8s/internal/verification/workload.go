package verification

import (
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Workload gives every role a separate offline Pod. The installation must
// enforce the included deny-all policy; the controller compares bounded output.
type Workload struct {
	ConfigMap, NetworkPolicy map[string]any
	Pods                     map[string]map[string]any
}

func Compile(c Command, in Inputs, image string, now time.Time) (Workload, error) {
	if executor.ValidatePlacement("ai-sdlc-verifiers", image, image) != nil || c.Recipe != LocaleRecipe || c.RecipeRevision != RecipeRevision() || !time.UnixMilli(c.ExpiresAt).After(now) || c.SourceSHA != in.delta.SourceSHA || c.CandidateTreeSHA256 != in.delta.CandidateTreeSHA256 {
		return Workload{}, ErrVerification
	}
	delta, err := candidate.Encode(in.delta)
	if err != nil || workspacebundle.Digest(delta) != c.DeltaSHA256 || !candidateID.MatchString(c.CandidateID) {
		return Workload{}, ErrVerification
	}
	name := "verify-" + c.ID()[13:45]
	configName := name + "-inputs"
	labels := map[string]any{"app.kubernetes.io/part-of": "ai-sdlc", "ai-sdlc.io/component": "verifier"}
	metadata := func(name string) map[string]any {
		return map[string]any{"name": name, "namespace": "ai-sdlc-verifiers", "labels": labels, "annotations": map[string]any{"ai-sdlc.io/command": c.ID(), "ai-sdlc.io/candidate": c.CandidateID, "ai-sdlc.io/recipe-revision": c.RecipeRevision}}
	}
	cases, _ := recipes.ReadFile("recipes/homedir-date-locale/cases.json")
	var publicCases []map[string]any
	if json.Unmarshal(cases, &publicCases) != nil {
		return Workload{}, ErrVerification
	}
	for _, item := range publicCases {
		delete(item, "locale")
	}
	publicRaw, _ := json.Marshal(publicCases)
	// Values are bytes to preserve exact source identities in binaryData.
	candidateDriver, _ := recipes.ReadFile("recipes/homedir-date-locale/candidate.cjs")
	referenceDriver, _ := recipes.ReadFile("recipes/homedir-date-locale/reference.cjs")
	data := map[string][]byte{"candidate-driver": candidateDriver, "reference-driver": referenceDriver, "baseline": in.files["baseline/"+LocaleSource], "candidate": in.files["candidate/"+LocaleSource], "cases": publicRaw, "reference-cases": cases, "existing-test": in.files["candidate/"+LocaleExistingTest], "new-test": in.files["candidate/"+LocaleNewTest]}
	for _, value := range data {
		if len(value) == 0 || len(value) > 32768 {
			return Workload{}, ErrVerification
		}
	}
	config := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": metadata(configName), "immutable": true, "binaryData": data}
	containers := []any{}
	volumes := []any{}
	for _, kind := range []string{"baseline", "candidate", "reference", "tests"} {
		items := []any{map[string]any{"key": "cases", "path": "cases.json"}}
		driverKey := "candidate-driver"
		if kind == "reference" {
			items = []any{map[string]any{"key": "reference-cases", "path": "cases.json"}}
			driverKey = "reference-driver"
		} else {
			items = append(items, map[string]any{"key": kind, "path": LocaleSource})
		}
		command := []string{"node", "/check/driver.cjs"}
		if kind == "tests" {
			items = []any{map[string]any{"key": "candidate", "path": LocaleSource}, map[string]any{"key": "existing-test", "path": LocaleExistingTest}, map[string]any{"key": "new-test", "path": LocaleNewTest}}
			command = []string{"node", "--test", "/check/" + LocaleExistingTest, "/check/" + LocaleNewTest}
		} else {
			items = append(items, map[string]any{"key": driverKey, "path": "driver.cjs"})
		}
		volumes = append(volumes, map[string]any{"name": kind, "configMap": map[string]any{"name": configName, "defaultMode": 0444, "items": items}})
		containers = append(containers, map[string]any{"name": kind, "image": image, "imagePullPolicy": "IfNotPresent", "command": command, "env": []any{map[string]any{"name": "TZ", "value": "UTC"}},
			"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000, "readOnlyRootFilesystem": true, "allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}},
			"resources":       map[string]any{"requests": map[string]any{"cpu": "50m", "memory": "64Mi"}, "limits": map[string]any{"cpu": "1", "memory": "256Mi", "ephemeral-storage": "16Mi"}},
			"volumeMounts":    []any{map[string]any{"name": kind, "mountPath": "/check", "readOnly": true}}})
	}
	pods := map[string]map[string]any{}
	for i, kind := range []string{"baseline", "candidate", "reference", "tests"} {
		pod := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": metadata(name + "-" + kind), "spec": map[string]any{"runtimeClassName": "ai-sdlc-offline", "automountServiceAccountToken": false, "enableServiceLinks": false, "restartPolicy": "Never", "activeDeadlineSeconds": 60, "terminationGracePeriodSeconds": 1, "hostNetwork": false, "hostPID": false, "hostIPC": false, "shareProcessNamespace": false,
			"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000, "fsGroup": 1000, "seccompProfile": map[string]any{"type": "RuntimeDefault"}}, "volumes": []any{volumes[i]}, "containers": []any{containers[i]}}}
		pods[kind] = pod
	}
	policy := map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": map[string]any{"name": "deny-all", "namespace": "ai-sdlc-verifiers"}, "spec": map[string]any{"podSelector": map[string]any{}, "policyTypes": []string{"Ingress", "Egress"}}}

	raw, err := json.Marshal(Workload{ConfigMap: config, Pods: pods, NetworkPolicy: policy})
	if err != nil {
		return Workload{}, ErrVerification
	}
	var w Workload
	if json.Unmarshal(raw, &w) != nil {
		return Workload{}, ErrVerification
	}
	return w, nil
}
