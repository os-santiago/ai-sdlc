package verification

import (
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repositorytests"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func installedService(t *testing.T, verified bool) {
	t.Helper()
	m := project.Manifest{Version: project.ManifestVersion, Repository: "example/service", BaseBranch: "main",
		Publication: project.Publication{Allow: []string{"**/*.go"}, MaxChangedFiles: 5, MaxChangedBytes: 65536, MaxFileBytes: 32768},
		Merge:       project.Merge{RulesetID: 1, Required: []project.Check{{Context: "ci", AppID: 1}}, StrictChecks: true, ResolveThreads: true}}
	if verified {
		m.Verification = &project.Verification{Image: "registry.example/go@sha256:" + strings.Repeat("a", 64), Command: []string{"go", "test", "./..."},
			TimeoutSeconds: 60, CPU: "1", Memory: "1Gi", EphemeralStorage: "1Gi", Network: "none", Result: project.Result{ExitCode: true},
			Repeat: 2, TestGlobs: []string{"**/*_test.go"}}
	} else {
		m.Recipes = []project.Recipe{{Name: "reviewed/v1", Marker: "AI-SDLC validation recipe: reviewed/v1"}}
	}
	project.SetInstalled(&m)
	t.Cleanup(func() { project.SetInstalled(nil) })
}

func TestAnInstalledManifestSelectsRepositoryTestsWithoutAMarker(t *testing.T) {
	installedService(t, true)
	recipe, selected := SelectRecipe("example/service", "Fix the reversal of multibyte strings.")
	if !selected || recipe != repositorytests.Recipe || !ValidRecipe("example/service", recipe) {
		t.Fatal("selection", recipe, selected)
	}
	if RevisionForRecipe(recipe) == "" || RevisionForRecipe(recipe) == RevisionForRecipe(LocaleRecipe) {
		t.Fatal("the recipe needs its own revision")
	}
	if _, selected := SelectRecipe("example/other", ""); selected {
		t.Fatal("a repository without an installed manifest selected a recipe")
	}
	if RecipeForRepository(project.Homedir) != LocaleRecipe {
		t.Fatal("reference repositories keep their recipes")
	}
}

func TestAManifestWithoutVerificationSelectsNothing(t *testing.T) {
	installedService(t, false)
	if _, selected := SelectRecipe("example/service", ""); selected {
		t.Fatal("selected without verification settings")
	}
}

func TestPrepareRepositoryTestsRequiresAnExactDelta(t *testing.T) {
	installedService(t, true)
	source := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: strings.Repeat("c", 40), Files: []workspacebundle.File{{Path: "a.go", Mode: 0o644, Data: []byte("old")}}}
	result := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: source.SourceSHA, Files: []workspacebundle.File{{Path: "a.go", Mode: 0o644, Data: []byte("new")}, {Path: "a_test.go", Mode: 0o644, Data: []byte("test")}}}
	d, err := candidate.Build(source, result)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := candidate.Encode(d)
	inputs, err := Prepare("example/service", repositorytests.Recipe, "", source, raw)
	if err != nil || len(inputs.Delta().Changes) != 2 {
		t.Fatal("prepare", err)
	}
	other := source
	other.Files = []workspacebundle.File{{Path: "a.go", Mode: 0o644, Data: []byte("different")}}
	if _, err := Prepare("example/service", repositorytests.Recipe, "", other, raw); err == nil {
		t.Fatal("a delta for another base accepted")
	}
	if _, err := Prepare("example/other", repositorytests.Recipe, "", source, raw); err == nil {
		t.Fatal("prepared for a repository without the recipe")
	}
}
