package mergegate

import (
	"io"
	"os"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

const DedicatedName = "AI-SDLC low-risk merge protection"

// LoadInstalled reads only a trusted configuration mount, never an issue or
// generated workspace. The returned ID must have been verified by the installer.
func LoadInstalled(path string) (Policy, error) {
	deny := func() (Policy, error) { return Policy{}, ErrGate }
	f, e := os.Open(path)
	if e != nil {
		return deny()
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, 16385))
	if e != nil || len(raw) > 16384 {
		return deny()
	}
	var p Policy
	if authority.DecodeStrict(raw, &p) != nil {
		return deny()
	}
	// Existing reviewed Homedir installations predate the explicit field.
	if p.Repository == "" {
		p.Repository = project.Homedir
	}
	if !p.valid() || p.Name != DedicatedName {
		return deny()
	}
	floor := PolicyForRepository(p.Repository)
	if len(p.Required) != len(floor.Required) {
		return deny()
	}
	for i, v := range floor.Required {
		if p.Required[i] != v {
			return deny()
		}
	}
	return p, nil
}

func LoadInstalledForRepository(path, repository string) (Policy, error) {
	p, err := LoadInstalled(path)
	if err != nil || p.Repository != repository {
		return Policy{}, ErrGate
	}
	return p, nil
}
