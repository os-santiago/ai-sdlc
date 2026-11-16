package buildworkload

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
)

type object = map[string]any

const BuildBudget = 10 * time.Minute
const PublishBudget = 5 * time.Minute
const ObservationMargin = time.Minute

// Compiled contains exact bytes to journal before the owning transport sends
// them. It also supplies the expected script to the separate admission profile.
type Compiled struct {
	Retained          RetainedContext
	Plan              building.Plan
	Build, Publish    []byte
	BuildScriptBase64 string
	ExpiresAt         time.Time
}

func (c Compiled) MayCreate(phase string, now time.Time) error {
	budget := BuildBudget + PublishBudget + ObservationMargin
	if phase == "publish" {
		budget = PublishBudget + ObservationMargin
	} else if phase != "build" {
		return ErrWorkload
	}
	if c.ExpiresAt.Sub(now) < budget {
		return ErrWorkload
	}
	return nil
}
func Compile(cfg building.Configuration, p Profile, signed authority.Receipt, payload []byte, slot building.Slot, now time.Time) (Compiled, error) {
	var out Compiled
	c, e := building.Verify(cfg.Inner, cfg.Outer, signed, payload, now)
	if e != nil || cfg.Validate() != nil || p.Validate() != nil || cfg.ProfileSHA256 != p.Digest() || c.ProfileSHA256 != cfg.ProfileDigest() || c.IntakeConfigSHA256 != cfg.IntakeConfigSHA256 || slot.Validate() != nil || slot.ClusterUID != p.ClusterUID || slot.NamespaceUID != p.NamespaceUID {
		return out, ErrWorkload
	}
	found := false
	for _, s := range cfg.Slots {
		if s == slot {
			found = true
		}
	}
	out.ExpiresAt = time.UnixMilli(c.ExpiresAt)
	if !found || out.MayCreate("build", now) != nil {
		return Compiled{}, ErrWorkload
	}
	compiled, e := compileDefinition(c, p, slot)
	if e != nil {
		return Compiled{}, e
	}
	retained := RetainedContext{Version: RetainedVersion, Compiler: CompilerVersion, Recipe: building.Recipe,
		Profile: p, Configuration: cfg.Binding(), IntakeConfigSHA256: cfg.IntakeConfigSHA256,
		Slot: slot, Build: compiled.Build, Publish: compiled.Publish}
	raw, e := retained.Encode()
	if e != nil {
		return Compiled{}, e
	}
	compiled.Retained, e = DecodeRetained(raw)
	if e != nil {
		return Compiled{}, e
	}
	return compiled, nil
}

// compileDefinition is private pure reconstruction, never an authority issuer.
// Historical validation uses it without obtaining an executable Compiled value.
func compileDefinition(c building.Command, p Profile, slot building.Slot) (Compiled, error) {
	var out Compiled
	out.ExpiresAt = time.UnixMilli(c.ExpiresAt)
	id := strings.TrimPrefix(c.ID(), "build-")[:32]
	output := building.Registry + ":" + id
	script := buildScript(p)
	build := pipeline(c, p, slot, "build", script, "DEFINITION", output)
	publish := pipeline(c, p, slot, "publish", script, "DEFINITION", output)
	// Hash an acyclic template with a literal definition placeholder, then insert
	// that hash into the exact requests. No caller supplies a manifest or script.
	template, _ := json.Marshal(object{"version": "ai-sdlc.saferus-build-definition/v1", "profileSHA256": p.Digest(), "slotDigest": slot.Digest(), "build": build, "publish": publish})
	definition := authority.Digest(template)
	build = pipeline(c, p, slot, "build", script, definition, output)
	publish = pipeline(c, p, slot, "publish", script, definition, output)
	out.Build, _ = json.Marshal(build)
	out.Publish, _ = json.Marshal(publish)
	if len(out.Build) > 32768 || len(out.Publish) > 32768 {
		return Compiled{}, ErrWorkload
	}
	out.Plan = building.Plan{Version: "ai-sdlc.build-plan/v1", BuildID: c.ID(), SourceSHA: c.SourceSHA, TreeSHA: c.TreeSHA, ProfileSHA256: c.ProfileSHA256, SlotDigest: slot.Digest(), DefinitionSHA256: definition, BuildRequestSHA256: authority.Digest(out.Build), PublishRequestSHA256: authority.Digest(out.Publish), PublisherImage: p.PublisherImage, Output: output}
	out.BuildScriptBase64 = base64.StdEncoding.EncodeToString([]byte(script))
	if out.Plan.Validate(c, slot) != nil {
		return Compiled{}, ErrWorkload
	}
	return out, nil
}
func security() object {
	return object{"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": object{"drop": []string{"ALL"}}, "seccompProfile": object{"type": "RuntimeDefault"}}
}
func resources(cpu, memory, storage string) object {
	return object{"requests": object{"cpu": "100m", "memory": "64Mi", "ephemeral-storage": "64Mi"}, "limits": object{"cpu": cpu, "memory": memory, "ephemeral-storage": storage}}
}
func pipeline(c building.Command, p Profile, s building.Slot, phase, script, definition, output string) object {
	readonly := phase == "publish"
	id := strings.TrimPrefix(c.ID(), "build-")[:32]
	guardArgs := []string{"--guard-workspace", "--storage-identity=" + s.StorageIdentitySHA256, "--node-boot-id=" + s.NodeBootID, "--workspace-phase=" + phase, "--authority-expires-at=" + strconv.FormatInt(c.ExpiresAt, 10)}
	steps := []object{{"name": "custody", "image": p.PublisherImage, "command": []string{"/component"}, "args": guardArgs, "securityContext": security(), "computeResources": resources("100m", "64Mi", "64Mi"), "timeout": "30s", "volumeMounts": []object{{"name": "slot", "mountPath": "/slot", "readOnly": readonly}}}}
	volumes := []object{{"name": "slot", "persistentVolumeClaim": object{"claimName": s.PVC, "readOnly": readonly}}}
	task := object{}
	spec := object{}
	budget := "10m"
	taskName := "build"
	if phase == "build" {
		volumes = append(volumes, object{"name": "tmp", "emptyDir": object{"medium": "Memory", "sizeLimit": "64Mi"}})
		steps = append(steps, object{"name": "verify", "image": GoImage, "script": script, "env": []object{{"name": "AI_SDLC_SOURCE_SHA", "value": c.SourceSHA}, {"name": "AI_SDLC_SOURCE_TREE", "value": c.TreeSHA}}, "securityContext": security(), "computeResources": resources("2", "2Gi", "2Gi"), "timeout": "9m", "volumeMounts": []object{{"name": "slot", "mountPath": "/slot"}, {"name": "tmp", "mountPath": "/tmp"}}})
	} else {
		budget = "5m"
		taskName = "publish"
		volumes = append(volumes, object{"name": "registry-auth", "secret": object{"secretName": "registry-auth", "defaultMode": 292}}, object{"name": "ca", "configMap": object{"name": "trusted-ca", "defaultMode": 292}})
		keys := []string{"IMAGE_URL", "IMAGE_DIGEST", "CHAINS-GIT_URL", "CHAINS-GIT_COMMIT", "DEFINITION_DIGEST", "RUN_UID"}
		results := []object{}
		pipelineResults := []object{}
		for _, key := range keys {
			results = append(results, object{"name": key, "type": "string"})
			pipelineResults = append(pipelineResults, object{"name": key, "value": "$(tasks.publish.results." + key + ")"})
		}
		task["params"] = []object{{"name": "run-uid", "value": "$(context.pipelineRun.uid)"}}
		spec["params"] = []object{{"name": "run-uid", "type": "string"}}
		spec["results"] = results
		steps = append(steps, object{"name": "publish", "image": p.PublisherImage, "command": []string{"/component"}, "args": []string{"--recipe=" + building.Recipe, "--input=/slot/workspace/output", "--output=" + output, "--source-sha=" + c.SourceSHA, "--definition-digest=sha256:" + definition, "--run-uid=$(params.run-uid)", "--timeout=4m"}, "securityContext": security(), "computeResources": resources("2", "2Gi", "512Mi"), "timeout": "4m30s", "volumeMounts": []object{{"name": "slot", "mountPath": "/slot", "readOnly": true}, {"name": "registry-auth", "mountPath": "/registry-auth", "readOnly": true}, {"name": "ca", "mountPath": "/etc/pki/ca-trust", "readOnly": true}}})
		task["pipelineResults"] = pipelineResults // moved into pipelineSpec below
	}
	spec["volumes"] = volumes
	spec["steps"] = steps
	task["name"] = taskName
	task["timeout"] = budget
	task["taskSpec"] = spec
	pipelineSpec := object{"tasks": []object{task}}
	if results, ok := task["pipelineResults"]; ok {
		delete(task, "pipelineResults")
		pipelineSpec["results"] = results
	}
	// No fsGroup: kubelet ownership rewriting would invalidate root-owned custody.
	podSecurity := object{"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000, "seccompProfile": object{"type": "RuntimeDefault"}}
	return object{"apiVersion": "tekton.dev/v1", "kind": "PipelineRun", "metadata": object{"name": "ai-sdlc-" + phase + "-" + id, "namespace": Namespace, "labels": object{"ai-sdlc.role": phase, "ai-sdlc.build-id": id}, "annotations": object{"ai-sdlc.build-id": c.ID(), "ai-sdlc.profile-sha256": c.ProfileSHA256, "ai-sdlc.slot-sha256": s.Digest(), "ai-sdlc.definition-sha256": definition, "ai-sdlc.expires-at": strconv.FormatInt(c.ExpiresAt, 10)}}, "spec": object{"timeouts": object{"pipeline": budget}, "taskRunTemplate": object{"serviceAccountName": phase, "podTemplate": object{"automountServiceAccountToken": false, "enableServiceLinks": false, "securityContext": podSecurity, "nodeSelector": object{"kubernetes.io/hostname": s.Node}}}, "pipelineSpec": pipelineSpec}}
}
func buildScript(p Profile) string {
	var gate strings.Builder
	for _, t := range p.DeniedTargets {
		fmt.Fprintf(&gate, `count=0
for attempt in {1..20}; do
  set +e
  timeout 3 bash -c 'exec 3<>/dev/tcp/$1/$2' probe %s %d
  status=$?
  set -e
  if [ "$status" -eq 124 ]; then count=$((count+1)); elif [ "$status" -eq 0 ]; then count=0; else exit 1; fi
  if [ "$count" -eq 2 ]; then break; fi
  sleep 1
done
test "$count" -eq 2
`, t.Host, t.Port)
	}
	return `#!/bin/bash
set -euo pipefail
umask 022
` + gate.String() + `test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token
test ! -e /registry-auth
mkdir -p /slot/workspace/cache /slot/workspace/modcache /slot/workspace/tmp /slot/workspace/output
export GOCACHE=/slot/workspace/cache GOMODCACHE=/slot/workspace/modcache TMPDIR=/slot/workspace/tmp
export GOTOOLCHAIN=local GOWORK=off GOFLAGS=-mod=readonly
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0
test "$(go env GOVERSION)" = go1.27.4
git -c core.hooksPath=/dev/null init /slot/workspace/source
cd /slot/workspace/source
git -c core.hooksPath=/dev/null -c protocol.file.allow=never fetch --no-tags --depth=1 https://github.com/os-santiago/saferus "$AI_SDLC_SOURCE_SHA"
git -c core.hooksPath=/dev/null checkout --detach FETCH_HEAD
test "$(git rev-parse HEAD)" = "$AI_SDLC_SOURCE_SHA"
test "$(git rev-parse 'HEAD^{tree}')" = "$AI_SDLC_SOURCE_TREE"
go vet ./...
go test -race ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /slot/workspace/output/saferus ./cmd/console
chmod 0755 /slot/workspace/output/saferus
`
}
