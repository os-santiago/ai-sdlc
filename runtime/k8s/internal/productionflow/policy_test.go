package productionflow

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"os"
	"path/filepath"
	"testing"
)

func TestProductionPolicyLoadsOriginalIdentityWithoutReleaseDeadlineInputs(t *testing.T) {
	m, e := authority.Bootstrap("os-santiago/saferus", "ai-sdlc-production")
	if e != nil {
		t.Fatal(e)
	}
	inner := m.Policy
	inner.Freeze = false
	inner.Autonomy = "auto-merge-low"
	inner.Grants = nil
	inner.Keys = map[string]authority.PublicKey{}
	outer := m.Policy
	outer.Freeze = false
	outer.Autonomy = "auto-deploy"
	outer.Grants = nil
	outer.Keys = map[string]authority.PublicKey{}
	for id, k := range m.Policy.Keys {
		if k.Role != "build-controller" {
			inner.Keys[id] = k
		}
		switch k.Role {
		case "release-manager", "build-controller", "verifier", "deployer", "relay", "orchestrator":
			outer.Keys[id] = k
		}
	}
	for _, g := range m.Policy.Grants {
		if authority.OuterOperation(g.Operation) {
			outer.Grants = append(outer.Grants, g)
		} else if g.Operation != "record-release" && g.Operation != "record-attempt" {
			inner.Grants = append(inner.Grants, g)
		}
	}
	path := filepath.Join(t.TempDir(), "outer.json")
	write := func(p authority.Policy) {
		raw, _ := json.Marshal(p)
		if os.WriteFile(path, raw, 0600) != nil {
			t.Fatal("fixture")
		}
	}
	write(outer)
	for _, role := range []string{"release-manager", "orchestrator"} {
		p, k, e := LoadOuter(inner, m.Identities[role], path, role)
		if e != nil || p.Revision() != outer.Revision() || k.KeyID != m.Identities[role].KeyID {
			t.Fatal("original current recording identity", e)
		}
	}
	for _, mode := range []string{"frozen", "missing-grant", "rotated-key", "wrong-role", "foreign-repo", "missing-file"} {
		t.Run(mode, func(t *testing.T) {
			p := outer
			p.Keys = map[string]authority.PublicKey{}
			for id, k := range outer.Keys {
				p.Keys[id] = k
			}
			p.Grants = append([]authority.Grant{}, outer.Grants...)
			role := "orchestrator"
			switch mode {
			case "frozen":
				p.Freeze = true
			case "missing-grant":
				var grants []authority.Grant
				for _, g := range p.Grants {
					if g.Operation != "record-production" {
						grants = append(grants, g)
					}
				}
				p.Grants = grants
			case "rotated-key":
				_, k, e := authority.NewIdentity("orchestrator", "orchestrator-1")
				if e != nil {
					t.Fatal(e)
				}
				p.Keys["orchestrator-1"] = k
			case "wrong-role":
				role = "worker"
			case "foreign-repo":
				p.Repository = "os-santiago/homedir"
			}
			write(p)
			if mode == "missing-file" {
				_ = os.Remove(path)
			}
			if _, _, e := LoadOuter(inner, m.Identities["orchestrator"], path, role); e == nil {
				t.Fatal("unreviewed authority")
			}
		})
	}
}
