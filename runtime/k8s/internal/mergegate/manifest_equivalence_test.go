package mergegate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

func TestBuiltinManifestsMatchCompiledMergePolicies(t *testing.T) {
	for _, repository := range []string{project.Homedir, project.Saferus} {
		m, ok := project.Builtin(repository)
		p := PolicyForRepository(repository, "")
		if !ok || p.Repository != repository || int64(p.RulesetID) != m.Merge.RulesetID || len(p.Required) != len(m.Merge.Required) {
			t.Fatal("merge policy divergence", repository)
		}
		var raw struct {
			StrictChecks, ResolveThreads bool
			HumanApprovals               int
			BypassActors                 []json.RawMessage
		}
		file := map[string][]byte{project.Homedir: pilotManifest, project.Saferus: saferusManifest}[repository]
		if json.Unmarshal(file, &raw) != nil || raw.StrictChecks != m.Merge.StrictChecks ||
			raw.ResolveThreads != m.Merge.ResolveThreads || raw.HumanApprovals != m.Merge.HumanApprovals || m.BaseBranch != "main" ||
			len(raw.BypassActors) != 0 {
			t.Fatal("merge rule divergence", repository)
		}
		for i, c := range p.Required {
			if c.Context != m.Merge.Required[i].Context || c.AppID != m.Merge.Required[i].AppID {
				t.Fatal("required check divergence", repository, c)
			}
		}
	}
}

func TestInstalledManifestProvidesTheMergePolicy(t *testing.T) {
	if p := PolicyForRepository("example/service", ""); p.Repository != "" {
		t.Fatal("unbound repository has a merge policy")
	}
	m, _ := project.Builtin(project.Saferus)
	m.Repository = "example/service"
	m.Merge.RulesetID = 42
	project.SetInstalled(&m)
	t.Cleanup(func() { project.SetInstalled(nil) })
	p := PolicyForRepository("example/service", m.Digest())
	if p.Repository != "example/service" || p.RulesetID != 42 || len(p.Required) != 1 || p.Required[0].Context != "quality" || !p.valid() {
		t.Fatal("manifest merge policy", p)
	}
	for name, change := range map[string]func(*project.Manifest){
		"approvals": func(m *project.Manifest) { m.Merge.HumanApprovals = 1 },
		"strict":    func(m *project.Manifest) { m.Merge.StrictChecks = false },
		"threads":   func(m *project.Manifest) { m.Merge.ResolveThreads = false },
		"branch":    func(m *project.Manifest) { m.BaseBranch = "develop" },
	} {
		unenforceable := m
		change(&unenforceable)
		project.SetInstalled(&unenforceable)
		if PolicyForRepository("example/service", unenforceable.Digest()).Repository != "" {
			t.Fatal("unenforceable merge value accepted:", name)
		}
	}
}

// The merge policy of a manifest repository comes only from the manifest the
// authorizing policy binds: another policy load's manifest, or a cleared
// registry, yields no policy and an installed gate is refused as ErrManifest.
func TestMergePolicyRefusesAnotherPolicyLoadsManifest(t *testing.T) {
	t.Cleanup(func() { project.SetInstalled(nil) })
	authorized, _ := project.Builtin(project.Saferus)
	authorized.Repository = "example/service"
	authorized.Merge.RulesetID = 42
	other := authorized
	other.Merge.Required = []project.Check{{Context: "other", AppID: 7}}
	digest := authorized.Digest()
	project.SetInstalled(&authorized)
	gate := PolicyForRepository("example/service", digest)
	gate.Name = DedicatedName
	file := filepath.Join(t.TempDir(), "policy.json")
	raw, _ := json.Marshal(gate)
	if os.WriteFile(file, raw, 0600) != nil {
		t.Fatal("fixture")
	}
	if got, e := LoadInstalledForRepository(file, "example/service", digest); e != nil || got.RulesetID != 42 {
		t.Fatal("the bound manifest's gate was refused", e)
	}
	// The gate file matching another load's manifest is refused too.
	otherGate := gate
	otherGate.Required = []RequiredCheck{{Context: "other", AppID: 7}}
	otherFile := filepath.Join(t.TempDir(), "policy.json")
	raw, _ = json.Marshal(otherGate)
	if os.WriteFile(otherFile, raw, 0600) != nil {
		t.Fatal("fixture")
	}
	for name, install := range map[string]*project.Manifest{"another load": &other, "cleared": nil} {
		project.SetInstalled(install)
		if p := PolicyForRepository("example/service", digest); p.Repository != "" {
			t.Fatalf("%s: merge policy from another manifest %+v", name, p)
		}
		for _, path := range []string{file, otherFile} {
			if _, e := LoadInstalledForRepository(path, "example/service", digest); !errors.Is(e, ErrManifest) {
				t.Fatalf("%s: installed gate accepted: %v", name, e)
			}
		}
	}
	// Reference repositories keep their compiled policies whatever is installed.
	project.SetInstalled(&other)
	if PolicyForRepository(project.Saferus, "").Repository != project.Saferus || PilotPolicy().Repository != project.Homedir {
		t.Fatal("reference merge policy changed")
	}
}
