package mergegate

import (
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
		for i, c := range p.Required {
			if c.Context != m.Merge.Required[i].Context || c.AppID != m.Merge.Required[i].AppID {
				t.Fatal("required check divergence", repository, c)
			}
		}
	}
}
