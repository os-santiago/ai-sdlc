package main

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"k8s.io/apimachinery/pkg/api/resource"
)

// compileProjectInstallation validates a reviewed project manifest against
// the installation policy's repository and returns its canonical form and
// digest, for the installer to bind into the policy and mount read-only. It
// generates no keys and touches no cluster, database or workflow.
func compileProjectInstallation(input io.Reader, output io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(input, 160*1024+1))
	if err != nil || len(raw) > 160*1024 {
		return errors.New("invalid project installation request")
	}
	var request struct {
		Policy   json.RawMessage `json:"policy"`
		Manifest json.RawMessage `json:"manifest"`
	}
	if authority.DecodeStrict(raw, &request) != nil {
		return errors.New("invalid project installation request")
	}
	policy, err := authority.ParsePolicy(request.Policy)
	if err != nil {
		return err
	}
	manifest, err := project.ParseManifest(request.Manifest)
	if err != nil || manifest.Repository != policy.Repository {
		return errors.New("project manifest does not match the installation repository")
	}
	if _, reference := project.Builtin(policy.Repository); reference {
		return errors.New("a reference repository uses its built-in manifest")
	}
	canonical := manifest.Canonical()
	quota, err := verifierQuota(manifest)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(struct {
		Version        string            `json:"version"`
		Manifest       string            `json:"manifestJSON"`
		ManifestSHA256 string            `json:"manifestSHA256"`
		VerifierQuota  map[string]string `json:"verifierQuota,omitempty"`
		ExecutorQuota  map[string]string `json:"executorQuota,omitempty"`
	}{"ai-sdlc.project-installation/v1", string(canonical), manifest.Digest(), quota, executorQuota(manifest)})
}

// verifierQuota sizes the verifier namespace for the manifest's three
// concurrent test runs. Each pod's effective requests are the tests
// container's plus the transfer sidecar's (the init copy is smaller); its
// limits add the sidecar's limits. The reference recipes need less.
func verifierQuota(m project.Manifest) (map[string]string, error) {
	if m.Verification == nil {
		return nil, nil
	}
	cpu, err := resource.ParseQuantity(m.Verification.CPU)
	if err != nil {
		return nil, errors.New("invalid verification CPU")
	}
	memory, err := resource.ParseQuantity(m.Verification.Memory)
	if err != nil {
		return nil, errors.New("invalid verification memory")
	}
	sum := func(base resource.Quantity, extra string) string {
		total := resource.Quantity{Format: base.Format}
		for i := 0; i < 3; i++ {
			total.Add(base)
			total.Add(resource.MustParse(extra))
		}
		return total.String()
	}
	return map[string]string{
		"requests.cpu": sum(cpu, "20m"), "requests.memory": sum(memory, "32Mi"),
		"limits.cpu": sum(cpu, "200m"), "limits.memory": sum(memory, "64Mi"),
	}, nil
}

// executorQuota sizes the executor namespace when the manifest declares a
// generation toolchain; without one the chart keeps its original quota.
func executorQuota(m project.Manifest) map[string]string {
	if m.Generation == nil || m.Generation.Toolchain == nil {
		return nil
	}
	return executor.ToolchainExecutorQuota()
}
