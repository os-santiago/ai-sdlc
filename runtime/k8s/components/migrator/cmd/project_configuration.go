package main

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
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
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(struct {
		Version        string `json:"version"`
		Manifest       string `json:"manifestJSON"`
		ManifestSHA256 string `json:"manifestSHA256"`
	}{"ai-sdlc.project-installation/v1", string(canonical), manifest.Digest()})
}
