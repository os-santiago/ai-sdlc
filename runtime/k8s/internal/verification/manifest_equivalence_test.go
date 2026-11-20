package verification

import (
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

// The built-in manifests express exactly the existing publication allowlists
// and recipes, so moving callers to manifests cannot widen or narrow scope.
func TestBuiltinManifestsMatchExistingAllowlistsAndRecipes(t *testing.T) {
	candidates := []string{LocaleSource, LocaleNewTest, LifetimeSource, LifetimeTest, LifetimeBrowser,
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
		if len(m.Recipes) != 1 || m.Recipes[0].Name != RecipeForRepository(repository) || m.Recipes[0].Marker != "required" {
			t.Fatal("recipe divergence", repository)
		}
	}
}
