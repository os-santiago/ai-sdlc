package authority

import (
	"os"
	"path/filepath"
	"testing"
)

// denyFixture mirrors spec/fixtures/component-authority.deny.v1.json. The
// committed file is the machine-readable record of issue #420's allow/deny
// fixture requirement; this test is its executor.
type denyFixture struct {
	Version     string `json:"version"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Base        struct {
		Freeze   bool   `json:"freeze"`
		Autonomy string `json:"autonomy"`
	} `json:"base"`
	ExerciseAllGrants bool `json:"exerciseAllGrants"`
	Cases             []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Set         *struct {
			Freeze   *bool  `json:"freeze"`
			Autonomy string `json:"autonomy"`
		} `json:"set"`
		GrantPatch *struct {
			Operation    string `json:"operation"`
			MinimumLevel string `json:"minimumLevel"`
			EventType    string `json:"eventType"`
		} `json:"grantPatch"`
		AppendGrant *Grant `json:"appendGrant"`
		KeyAlias    *struct {
			From string `json:"from"`
			To   string `json:"to"`
			Role string `json:"role"`
		} `json:"keyAlias"`
		Call struct {
			Caller      string `json:"caller"`
			Target      string `json:"target"`
			Operation   string `json:"operation"`
			Repository  string `json:"repository"`
			Environment string `json:"environment"`
		} `json:"call"`
	} `json:"cases"`
}

func TestCommittedDenialFixtures(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "..", "spec", "fixtures")
	raw, err := os.ReadFile(filepath.Join(dir, "component-authority.deny.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture denyFixture
	if err := DecodeStrict(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != "1" || fixture.Source != "component-authority.v1.json" {
		t.Fatal("unsupported denial fixture or source")
	}
	source, err := os.ReadFile(filepath.Join(dir, fixture.Source))
	if err != nil {
		t.Fatal(err)
	}
	base, err := ParsePolicy(source)
	if err != nil {
		t.Fatal(err)
	}
	base.Freeze = fixture.Base.Freeze
	base.Autonomy = fixture.Base.Autonomy
	if fixture.ExerciseAllGrants {
		for _, grant := range base.Grants {
			if err := base.Allows(grant.Caller, grant.Target, grant.Operation, base.Repository, base.Environment); err != nil {
				t.Fatalf("positive control: declared transition %s denied: %v", grant.Operation, err)
			}
		}
	}
	seen := map[string]bool{}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if seen[c.Name] {
				t.Fatal("duplicate fixture case name")
			}
			seen[c.Name] = true
			p := base
			p.Grants = append([]Grant(nil), base.Grants...)
			p.Keys = map[string]PublicKey{}
			for id, key := range base.Keys {
				p.Keys[id] = key
			}
			if c.Set != nil {
				if c.Set.Freeze != nil {
					p.Freeze = *c.Set.Freeze
				}
				if c.Set.Autonomy != "" {
					p.Autonomy = c.Set.Autonomy
				}
			}
			if c.GrantPatch != nil {
				for i := range p.Grants {
					if p.Grants[i].Operation == c.GrantPatch.Operation {
						if c.GrantPatch.MinimumLevel != "" {
							p.Grants[i].MinimumLevel = c.GrantPatch.MinimumLevel
						}
						if c.GrantPatch.EventType != "" {
							p.Grants[i].EventType = c.GrantPatch.EventType
						}
					}
				}
			}
			if c.AppendGrant != nil {
				p.Grants = append(p.Grants, *c.AppendGrant)
			}
			if c.KeyAlias != nil {
				key := p.Keys[c.KeyAlias.From]
				key.Role = c.KeyAlias.Role
				p.Keys[c.KeyAlias.To] = key
			}
			repo, env := c.Call.Repository, c.Call.Environment
			if repo == "" {
				repo = p.Repository
			}
			if env == "" {
				env = p.Environment
			}
			if err := p.Allows(c.Call.Caller, c.Call.Target, c.Call.Operation, repo, env); err == nil {
				t.Fatalf("denial fixture %q was allowed", c.Name)
			}
		})
	}
}
