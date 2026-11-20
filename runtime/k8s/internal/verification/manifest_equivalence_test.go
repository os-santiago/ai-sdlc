package verification

import (
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

// The built-in manifests express exactly the existing publication allowlists
// and recipes, so moving callers to manifests cannot widen or narrow scope.
func TestBuiltinManifestsMatchExistingAllowlistsAndRecipes(t *testing.T) {
	candidates := []string{"Web/static/ttl.js", "web//static/ttl.js", LocaleSource, LocaleNewTest, LifetimeSource, LifetimeTest, LifetimeBrowser,
		"web/static/other.js", "tests/js/other.test.js", "README.md", ".github/workflows/ci.yml",
		"web/static/ttl.js/", "../web/static/ttl.js", "tools/verify_browser.py.bak", "quarkus-app/pom.xml"}
	for _, repository := range []string{project.Homedir, project.Saferus} {
		m, ok := project.Builtin(repository)
		if !ok {
			t.Fatal("missing built-in", repository)
		}
		for _, file := range candidates {
			if m.Publication.Allows(file) != AllowedPublicationPath(repository, file) {
				t.Fatal("allowlist divergence", repository, file)
			}
		}
		// Exact literals: no wildcard can widen the reference scope.
		want := map[string][]string{project.Homedir: {LocaleSource, LocaleNewTest}, project.Saferus: {LifetimeSource, LifetimeTest, LifetimeBrowser}}[repository]
		if len(m.Publication.Allow) != len(want) || len(m.Publication.Deny) != 0 {
			t.Fatal("unexpected built-in patterns", repository)
		}
		for i, pattern := range m.Publication.Allow {
			if pattern != want[i] || strings.ContainsAny(pattern, "*?") {
				t.Fatal("built-in pattern is not the exact allowed path", repository, pattern)
			}
		}
		if len(m.Recipes) != 1 || m.Recipes[0].Name != RecipeForRepository(repository) || m.Recipes[0].Marker != "required" {
			t.Fatal("recipe divergence", repository)
		}
	}
}
