package buildworkload

import (
	"fmt"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"strings"
)

type mount struct {
	name, path, sub string
	readonly        bool
}

func mountChecks() []string {
	checks := []string{}
	for _, phase := range []string{"build", "publish"} {
		for _, container := range []string{"prepare", "place-scripts", "step-custody", "step-verify", "step-publish"} {
			if phase == "build" && container == "step-publish" || phase == "publish" && (container == "step-verify" || container == "place-scripts") {
				continue
			}
			mounts := []mount{}
			add := func(name, path string, ro bool) { mounts = append(mounts, mount{name: name, path: path, readonly: ro}) }
			if container == "prepare" {
				add("tekton-internal-bin", "/tekton/bin", false)
				add("tekton-internal-steps", "/tekton/steps", false)
			} else if container == "place-scripts" {
				add("tekton-internal-scripts", "/tekton/scripts", false)
				add("tekton-internal-bin", "/tekton/bin", false)
			} else {
				add("slot", "/slot", phase == "publish")
				if container == "step-custody" {
					add("tekton-internal-downward", "/tekton/downward", true)
				}
				if container == "step-verify" {
					add("tmp", "/tmp", false)
					add("tekton-internal-scripts", "/tekton/scripts", true)
				}
				if container == "step-publish" {
					add("registry-auth", "/registry-auth", true)
					add("ca", "/etc/pki/ca-trust", true)
				}
				index := "1"
				if container == "step-custody" {
					index = "0"
				}
				add("tekton-creds-init-home-"+index, "/tekton/creds", false)
				add("tekton-internal-run-0", "/tekton/run/0", index != "0")
				add("tekton-internal-run-1", "/tekton/run/1", index != "1")
				add("tekton-internal-bin", "/tekton/bin", true)
				add("tekton-internal-workspace", "/workspace", false)
				for _, n := range []string{"home", "results", "steps", "artifacts"} {
					add("tekton-internal-"+n, "/tekton/"+n, n == "steps")
				}
				if phase == "publish" {
					add("DYNAMIC-CREDS", "/tekton/creds-secrets/registry-auth", false)
				}
				mounts = append(mounts, mount{"config-trusted-cabundle-volume", "/tekton-custom-certs/ca-bundle.crt", "ca-bundle.crt", true}, mount{"config-service-cabundle-volume", "/tekton-custom-certs/service-ca.crt", "service-ca.crt", true})
			}
			terms := []string{}
			for _, m := range mounts {
				name := "m.name == " + cel(m.name)
				if m.name == "DYNAMIC-CREDS" {
					name = "m.name.matches('^tekton-creds-[a-f0-9]{8}$')"
				}
				ro := "(!has(m.readOnly) || !m.readOnly)"
				if m.readonly {
					ro = "has(m.readOnly) && m.readOnly"
				}
				sub := "!has(m.subPath)"
				if m.sub != "" {
					sub = "m.subPath == " + cel(m.sub)
				}
				terms = append(terms, "("+name+" && m.mountPath == "+cel(m.path)+" && "+ro+" && "+sub+" && !has(m.subPathExpr) && (!has(m.mountPropagation) || m.mountPropagation == 'None'))")
			}
			checks = append(checks, fmt.Sprintf("variables.phase != %s || (object.spec.containers + object.spec.initContainers).filter(c,c.name == %s).all(c, c.volumeMounts.size() == %d && c.volumeMounts.all(m, %s))", cel(phase), cel(container), len(mounts), strings.Join(terms, " || ")))
		}
	}
	return checks
}
func resourceChecks() []string {
	checks := []string{}
	for _, name := range []string{"prepare", "place-scripts", "step-custody", "step-verify", "step-publish"} {
		cpu, memory, storage := "100m", "64Mi", "64Mi"
		switch name {
		case "prepare", "place-scripts":
			memory = "256Mi"
		case "step-verify":
			cpu, memory, storage = "2", "2Gi", "2Gi"
		case "step-publish":
			cpu, memory, storage = "2", "2Gi", "512Mi"
		}
		checks = append(checks, "(object.spec.containers + object.spec.initContainers).filter(c,c.name == "+cel(name)+").all(c, has(c.resources) && dyn(c.resources).limits == "+cel(map[string]string{"cpu": cpu, "memory": memory, "ephemeral-storage": storage})+" && dyn(c.resources).requests == "+cel(map[string]string{"cpu": "100m", "memory": "64Mi", "ephemeral-storage": "64Mi"})+")")
	}
	return checks
}
func publisherArguments() string {
	prefix := []string{"-wait_file", "/tekton/run/0/out", "-post_file", "/tekton/run/1/out", "-termination_path", "/tekton/termination", "-step_metadata_dir", "/tekton/run/1/status", "-docker-config=registry-auth", "-timeout", "4m30s", "-results", resultNames, "-entrypoint", "/component", "--", "--recipe=" + building.Recipe, "--input=/slot/workspace/output"}
	terms := []string{"size(c.args) == 23"}
	for i, v := range prefix {
		terms = append(terms, fmt.Sprintf("c.args[%d] == %s", i, cel(v)))
	}
	terms = append(terms, "c.args[18] == "+cel("--output="+building.Registry+":")+" + object.metadata.labels['ai-sdlc.build-id']", "c.args[19].matches('^--source-sha=[a-f0-9]{40}$')", "c.args[20] == '--definition-digest=sha256:' + object.metadata.annotations['ai-sdlc.definition-sha256']", "c.args[21] == '--run-uid=' + object.metadata.labels['tekton.dev/pipelineRunUID']", "c.args[22] == '--timeout=4m'")
	return "object.spec.containers.filter(c,c.name == 'step-publish').all(c, " + strings.Join(terms, " && ") + ")"
}
