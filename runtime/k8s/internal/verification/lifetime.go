package verification

import (
	"bytes"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repositorytests"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

const LifetimeRecipe = "saferus-token-lifetime/v1"
const LifetimeSource = "web/static/ttl.js"
const LifetimeTest = "tests/ttl.test.mjs"
const LifetimeBrowser = "tools/verify_browser.py"
const LifetimeMarker = "AI-SDLC validation recipe: " + LifetimeRecipe

func RecipeForRepository(repository string) string {
	switch repository {
	case project.Homedir:
		return LocaleRecipe
	case project.Saferus:
		return LifetimeRecipe
	}
	// Any other repository is verified with its own tests when the policy
	// binds an installed manifest that declares them.
	if m, ok := project.Lookup(repository); ok && m.Verification != nil {
		return repositorytests.Recipe
	}
	return ""
}

func ValidRecipe(repository, recipe string) bool {
	return recipe != "" && RecipeForRepository(repository) == recipe
}

func SelectRecipe(repository, body string) (string, bool) {
	recipe := RecipeForRepository(repository)
	if recipe == repositorytests.Recipe {
		// The installed manifest selects it; the issue needs no marker.
		return recipe, true
	}
	return recipe, recipe != "" && selectMarker(body, "AI-SDLC validation recipe: "+recipe)
}

func recipeDirectory(recipe string) string {
	switch recipe {
	case LocaleRecipe:
		return "recipes/homedir-date-locale"
	case LifetimeRecipe:
		return "recipes/saferus-token-lifetime"
	default:
		return ""
	}
}

func AllowedPublicationPath(repository, path string) bool {
	switch repository {
	case project.Homedir:
		return path == LocaleSource || path == LocaleNewTest
	case project.Saferus:
		return path == LifetimeSource || path == LifetimeTest || path == LifetimeBrowser
	default:
		return false
	}
}

func prepareLifetime(body string, source workspacebundle.Bundle, raw []byte) (Inputs, error) {
	if !selectMarker(body, LifetimeMarker) {
		return Inputs{}, ErrVerification
	}
	delta, err := candidate.Decode(raw)
	if err != nil || len(delta.Changes) != 3 {
		return Inputs{}, ErrVerification
	}
	result, err := candidate.Apply(source, delta)
	if err != nil {
		return Inputs{}, ErrVerification
	}
	var browserSource []byte
	for _, file := range source.Files {
		if file.Path == LifetimeBrowser {
			browserSource = file.Data
		}
	}
	seen := map[string]bool{}
	for _, change := range delta.Changes {
		if !AllowedPublicationPath(project.Saferus, change.Path) || seen[change.Path] || change.Before == nil || change.After == nil || change.After.Mode != 0644 || change.Before.Mode != 0644 || len(change.After.Data) == 0 || len(change.After.Data) > 32768 {
			return Inputs{}, ErrVerification
		}
		seen[change.Path] = true
		if change.Path == LifetimeBrowser {
			// Browser TLS/authentication code is outside generative authority.
			// Only this exact presentation assertion may follow the UI change.
			old := []byte("expect(page.locator('#ttl')).to_have_text('60 seconds')")
			updated := []byte("expect(page.locator('#ttl')).to_have_text('1 minute')")
			if bytes.Count(browserSource, old) != 1 || !bytes.Equal(change.After.Data, bytes.Replace(browserSource, old, updated, 1)) {
				return Inputs{}, ErrVerification
			}
		}
	}
	inputs := Inputs{delta: delta, files: map[string][]byte{}}
	for prefix, bundle := range map[string]workspacebundle.Bundle{"baseline/": source, "candidate/": result} {
		for _, file := range bundle.Files {
			if AllowedPublicationPath(project.Saferus, file.Path) {
				if file.Mode != 0644 || len(file.Data) > 32768 {
					return Inputs{}, ErrVerification
				}
				inputs.files[prefix+file.Path] = bytes.Clone(file.Data)
			}
		}
	}
	if len(inputs.files) != 6 {
		return Inputs{}, ErrVerification
	}
	return inputs, nil
}
