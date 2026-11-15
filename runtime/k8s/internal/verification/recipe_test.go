package verification

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func recipeFixture(t *testing.T) (workspacebundle.Bundle, []byte) {
	t.Helper()
	source := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: strings.Repeat("a", 40), Files: []workspacebundle.File{{Path: LocaleSource, Mode: 0644, Data: []byte("source")}, {Path: LocaleExistingTest, Mode: 0644, Data: []byte("existing test")}}}
	result := source
	result.Files = append([]workspacebundle.File{}, source.Files...)
	result.Files[0].Data = []byte("candidate")
	result.Files = append(result.Files, workspacebundle.File{Path: LocaleNewTest, Mode: 0644, Data: []byte("regression test")})
	d, err := candidate.Build(source, result)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := candidate.Encode(d)
	if err != nil {
		t.Fatal(err)
	}
	return source, raw
}
func TestRecipeReconstructsScopeWithoutTrustingGeneratedTests(t *testing.T) {
	source, raw := recipeFixture(t)
	in, err := Prepare("os-santiago/homedir", LocaleRecipe, RecipeMarker, source, raw)
	if err != nil || len(in.files) != 5 || !workspacebundle.ValidDigest(RecipeRevision()) {
		t.Fatal("reviewed recipe", err)
	}
	for _, body := range []string{"", RecipeMarker + "\n" + RecipeMarker, "AI-SDLC validation recipe: arbitrary-shell/v1"} {
		if _, err := Prepare("os-santiago/homedir", LocaleRecipe, body, source, raw); err == nil {
			t.Fatal("ambiguous or unsupported issue recipe")
		}
	}

	for _, path := range []string{".github/workflows/publish.yml", "AGENTS.md", "pom.xml", "tests/js/other.test.js"} {
		result := source
		result.Files = append([]workspacebundle.File{}, source.Files...)
		result.Files[0].Data = []byte("modified source")
		result.Files = append(result.Files, workspacebundle.File{Path: path, Mode: 0644, Data: []byte("scope substitution")})
		delta, err := candidate.Build(source, result)
		if err != nil {
			t.Fatal(err)
		}
		mutated, err := candidate.Encode(delta)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = Prepare("os-santiago/homedir", LocaleRecipe, RecipeMarker, source, mutated); err == nil {
			t.Fatal("valid delta escaped reviewed scope")
		}
	}

	d, _ := candidate.Decode(raw)
	d.SourceSHA = strings.Repeat("b", 40)
	raw, _ = candidate.Encode(d)
	if _, err := Prepare("os-santiago/homedir", LocaleRecipe, RecipeMarker, source, raw); err == nil {
		t.Fatal("base substitution")
	}
}
func TestResultsCannotClaimSuccessOrHideMismatch(t *testing.T) {
	values := make([]string, 15)
	for i := range values {
		values[i] = "expected"
	}
	good, _ := json.Marshal(values)
	baseline := append([]string{}, values...)
	baseline[0] = ""
	before, _ := json.Marshal(baseline)
	if Compare(before, good, good) != nil {
		t.Fatal("expected oracle comparison")
	}
	for _, raw := range [][]byte{[]byte(`{"success":true}`), append(append([]byte{}, good...), []byte(" trailing")...), []byte(`[null]`), []byte(`[]`), []byte(strings.Repeat("x", 32769)), before} {
		if Compare(before, raw, good) == nil {
			t.Fatal("untrusted result minted success")
		}
	}
	if Compare(good, good, good) == nil {
		t.Fatal("baseline defect not reproduced")
	}
	values[0] = "changed"
	changed, _ := json.Marshal(values)
	if Compare(before, changed, good) == nil {
		t.Fatal("behavior mismatch ignored")
	}
}

func TestCompilerIsolatesOracleAndCandidateInputs(t *testing.T) {
	source, raw := recipeFixture(t)
	inputs, err := Prepare("os-santiago/homedir", LocaleRecipe, RecipeMarker, source, raw)
	if err != nil {
		t.Fatal(err)
	}
	command := Command{CandidateID: "execution-" + strings.Repeat("a", 64), Recipe: LocaleRecipe, RecipeRevision: RecipeRevision(), SourceSHA: source.SourceSHA, CandidateTreeSHA256: inputs.delta.CandidateTreeSHA256, DeltaSHA256: workspacebundle.Digest(raw), ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	w, err := Compile(command, inputs, "example/runner@sha256:"+strings.Repeat("b", 64), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Pods) != 4 {
		t.Fatal("roles share a Pod")
	}
	for kind, pod := range w.Pods {
		spec := pod["spec"].(map[string]any)
		if len(spec["containers"].([]any)) != 1 || spec["containers"].([]any)[0].(map[string]any)["name"] != kind {
			t.Fatal("role isolation")
		}
		if spec["automountServiceAccountToken"] != false || spec["shareProcessNamespace"] != false || spec["runtimeClassName"] != "ai-sdlc-offline" {
			t.Fatal("oracle isolation missing")
		}
		volumes := spec["volumes"].([]any)
		for _, rawVolume := range volumes {
			volume := rawVolume.(map[string]any)
			name := volume["name"].(string)
			items := volume["configMap"].(map[string]any)["items"].([]any)
			for _, rawItem := range items {
				key := rawItem.(map[string]any)["key"].(string)
				if name == "reference" && (key == "candidate" || key == "baseline" || key == "candidate-driver") {
					t.Fatal("reference consumes untrusted executable")
				}
				if name != "reference" && (key == "reference-cases" || key == "reference-driver") {
					t.Fatal("untrusted container sees expected oracle inputs")
				}
			}
		}
	}
	command.DeltaSHA256 = strings.Repeat("0", 64)
	if _, err = Compile(command, inputs, "example/runner@sha256:"+strings.Repeat("b", 64), time.Now()); err == nil {
		t.Fatal("different candidate compiled")
	}
}
