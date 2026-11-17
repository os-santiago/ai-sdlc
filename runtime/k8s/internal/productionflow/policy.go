package productionflow

import (
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"io"
	"os"
)

// LoadOuter reads current recording authority only. Historic deployment and
// trust deadlines are checked by the immutable fact reader, not renewed here.
func LoadOuter(inner authority.Policy, key authority.Identity, path, role string) (authority.Policy, authority.Identity, error) {
	if role != "release-manager" && role != "orchestrator" || key.Role != role || key.Validate(inner) != nil {
		return authority.Policy{}, authority.Identity{}, ErrFlow
	}
	f, e := os.Open(path)
	if e != nil {
		return authority.Policy{}, authority.Identity{}, ErrFlow
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, 32769))
	if e != nil || len(raw) > 32768 {
		return authority.Policy{}, authority.Identity{}, ErrFlow
	}
	p, e := authority.ParsePolicy(raw)
	if e != nil || authority.ValidateLoopPolicies(inner, p) != nil || inner.Freeze || p.Freeze || key.Validate(p) != nil || p.Allows("release-manager", "orchestrator", "record-production", p.Repository, p.Environment) != nil {
		return authority.Policy{}, authority.Identity{}, ErrFlow
	}
	return p, key, nil
}
