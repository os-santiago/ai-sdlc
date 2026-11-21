// Package verification independently reconstructs candidates and selects finite,
// reviewed behavioral recipes. This package grants no publication authority.
package verification

import (
	"bytes"
	"embed"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"strings"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repositorytests"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

const LocaleRecipe = "homedir-date-locale/v1"
const LocaleSource = "quarkus-app/src/main/resources/META-INF/resources/js/utils.js"
const LocaleExistingTest = "tests/js/utils-format-date.test.js"
const LocaleNewTest = "tests/js/utils-locale-fallback.test.js"
const RecipeMarker = "AI-SDLC validation recipe: " + LocaleRecipe

var ErrVerification = errors.New("candidate verification denied")

//go:embed recipes/homedir-date-locale/* recipes/saferus-token-lifetime/*
var recipes embed.FS

// Inputs are copies reconstructed from an independently fetched complete base.
// No executable command or trusted test is accepted from a candidate or issue.
type Inputs struct {
	delta candidate.Delta
	files map[string][]byte
}

// Delta returns the validated candidate delta.
func (i Inputs) Delta() candidate.Delta { return i.delta }

func Prepare(repository, recipe, issueBody string, source workspacebundle.Bundle, raw []byte) (Inputs, error) {
	if recipe == repositorytests.Recipe && ValidRecipe(repository, recipe) {
		// The candidate must apply exactly to the fetched base; the trees and
		// the decision are built by the repository-tests flow.
		d, err := candidate.Decode(raw)
		if err != nil {
			return Inputs{}, ErrVerification
		}
		if _, err := candidate.Apply(source, d); err != nil {
			return Inputs{}, ErrVerification
		}
		return Inputs{delta: d}, nil
	}
	if repository == "os-santiago/saferus" && recipe == LifetimeRecipe {
		return prepareLifetime(issueBody, source, raw)
	}
	if repository != "os-santiago/homedir" || recipe != LocaleRecipe || len(issueBody) > 65536 {
		return Inputs{}, ErrVerification
	}
	if !SelectLocaleRecipe(issueBody) {
		return Inputs{}, ErrVerification
	}
	d, err := candidate.Decode(raw)
	if err != nil {
		return Inputs{}, ErrVerification
	}
	result, err := candidate.Apply(source, d)
	if err != nil || len(d.Changes) != 2 {
		return Inputs{}, ErrVerification
	}
	seen := map[string]bool{}
	for _, change := range d.Changes {
		if change.Path != LocaleSource && change.Path != LocaleNewTest || change.After == nil || change.After.Mode != 0644 || len(change.After.Data) > 32768 || len(change.After.Data) == 0 || (change.Path == LocaleSource && change.Before == nil) || (change.Path == LocaleNewTest && change.Before != nil) {
			return Inputs{}, ErrVerification
		}
		seen[change.Path] = true
	}
	if !seen[LocaleSource] || !seen[LocaleNewTest] {
		return Inputs{}, ErrVerification
	}
	inputs := Inputs{delta: d, files: map[string][]byte{}}
	for _, f := range source.Files {
		if f.Path == LocaleSource || f.Path == LocaleExistingTest {
			if f.Mode != 0644 || len(f.Data) > 32768 {
				return Inputs{}, ErrVerification
			}
			inputs.files["baseline/"+f.Path] = bytes.Clone(f.Data)
		}
	}
	for _, f := range result.Files {
		if f.Path == LocaleSource || f.Path == LocaleExistingTest || f.Path == LocaleNewTest {
			inputs.files["candidate/"+f.Path] = bytes.Clone(f.Data)
		}
	}
	if len(inputs.files) != 5 {
		return Inputs{}, ErrVerification
	}
	return inputs, nil
}

// RecipeRevision binds the exact reviewed oracle and case set, separately from
// generated test bytes and the generation profile revision.
func RecipeRevision() string {
	return RevisionForRecipe(LocaleRecipe)
}

func RevisionForRecipe(recipe string) string {
	if recipe == repositorytests.Recipe {
		// The settings are bound by the policy's project digest; the revision
		// binds the decision rules.
		return workspacebundle.Digest([]byte(repositorytests.Recipe + "\x00" + repositorytests.RulesVersion))
	}
	directory := recipeDirectory(recipe)
	if directory == "" {
		return ""
	}
	var raw []byte
	for _, name := range []string{"candidate.cjs", "reference.cjs", "cases.json"} {
		v, _ := recipes.ReadFile(directory + "/" + name)
		raw = append(raw, []byte(name+"\x00")...)
		raw = append(raw, v...)
		raw = append(raw, 0)
	}
	return workspacebundle.Digest(raw)
}
func Results(raw []byte) ([]string, error) {
	return recipeResults(LocaleRecipe, raw)
}

func recipeResults(recipe string, raw []byte) ([]string, error) {
	count := 15
	if recipe == LifetimeRecipe {
		count = 20
	} else if recipe != LocaleRecipe {
		return nil, ErrVerification
	}
	if len(raw) == 0 || len(raw) > 32768 {
		return nil, ErrVerification
	}
	var decoded []any
	if authority.DecodeStrict(raw, &decoded) != nil || len(decoded) != count {
		return nil, ErrVerification
	}
	values := make([]string, len(decoded))
	for i, value := range decoded {
		text, ok := value.(string)
		if !ok || len(text) > 1024 {
			return nil, ErrVerification
		}
		values[i] = text
	}
	return values, nil
}
func Compare(baseline, actual, reference []byte) error {
	return CompareRecipe(LocaleRecipe, baseline, actual, reference)
}

func CompareRecipe(recipe string, baseline, actual, reference []byte) error {
	b, e := recipeResults(recipe, baseline)
	if e != nil {
		return e
	}
	a, e := recipeResults(recipe, actual)
	if e != nil {
		return e
	}
	r, e := recipeResults(recipe, reference)
	if e != nil {
		return e
	}
	if (recipe == LocaleRecipe && (b[0] != "" || r[0] == "")) || (recipe == LifetimeRecipe && (b[0] != "60 seconds" || r[0] != "1 minute")) {
		return ErrVerification
	}
	for i := range a {
		if a[i] != r[i] {
			return ErrVerification
		}
	}
	return nil
}

// SelectLocaleRecipe accepts only one exact reviewed issue marker.
func SelectLocaleRecipe(issueBody string) bool {
	return selectMarker(issueBody, RecipeMarker)
}

func selectMarker(issueBody, marker string) bool {
	if len(issueBody) > 65536 {
		return false
	}
	count := 0
	for _, line := range strings.Split(issueBody, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "AI-SDLC validation recipe:") && line != marker {
			return false
		}
		if line == marker {
			count++
		}
	}
	if count != 1 {
		return false
	}
	return true
}
