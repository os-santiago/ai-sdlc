package mergegate

import (
	"encoding/json"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

func TestBuiltinManifestsMatchCompiledMergePolicies(t *testing.T) {
	for _, repository := range []string{project.Homedir, project.Saferus} {
		m, ok := project.Builtin(repository)
		p := PolicyForRepository(repository)
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
	if p := PolicyForRepository("example/service"); p.Repository != "" {
		t.Fatal("unbound repository has a merge policy")
	}
	m, _ := project.Builtin(project.Saferus)
	m.Repository = "example/service"
	m.Merge.RulesetID = 42
	project.SetInstalled(&m)
	t.Cleanup(func() { project.SetInstalled(nil) })
	p := PolicyForRepository("example/service")
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
		if PolicyForRepository("example/service").Repository != "" {
			t.Fatal("unenforceable merge value accepted:", name)
		}
	}
}
