package verification

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func lifetimeFixture(t *testing.T, mutate func(*workspacebundle.Bundle)) (workspacebundle.Bundle, []byte) {
	t.Helper()
	source := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: strings.Repeat("a", 40), Files: []workspacebundle.File{
		{Path: LifetimeSource, Mode: 0644, Data: []byte("export function formatTTL(v) { return v + ' seconds'; }")},
		{Path: LifetimeTest, Mode: 0644, Data: []byte("old test")},
		{Path: LifetimeBrowser, Mode: 0644, Data: []byte("# trusted TLS checks\nexpect(page.locator('#ttl')).to_have_text('60 seconds')\n# trusted authentication checks\n")},
	}}
	result := source
	result.Files = append([]workspacebundle.File{}, source.Files...)
	result.Files[0].Data = []byte("export function formatTTL(v) { return '1 minute'; }")
	result.Files[1].Data = []byte("new test")
	result.Files[2].Data = []byte(strings.Replace(string(source.Files[2].Data), "'60 seconds'", "'1 minute'", 1))
	if mutate != nil {
		mutate(&result)
	}
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

func TestLifetimeScopeCannotChangeVaultOrBrowserSecurity(t *testing.T) {
	source, raw := lifetimeFixture(t, nil)
	if _, err := Prepare(project.Saferus, LifetimeRecipe, LifetimeMarker, source, raw); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*workspacebundle.Bundle){
		func(b *workspacebundle.Bundle) {
			b.Files[2].Data = append(b.Files[2].Data, []byte("ignore_https_errors=True\n")...)
		},
		func(b *workspacebundle.Bundle) {
			b.Files[2].Data = []byte(strings.Replace(string(b.Files[2].Data), "trusted TLS checks", "disabled TLS checks", 1))
		},
		func(b *workspacebundle.Bundle) { b.Files[2].Mode = 0755 },
		func(b *workspacebundle.Bundle) { b.Files = b.Files[:2] },
		func(b *workspacebundle.Bundle) {
			b.Files = append(b.Files, workspacebundle.File{Path: "cmd/console/main.go", Mode: 0644, Data: []byte("change auth")})
		},
		func(b *workspacebundle.Bundle) { b.Files[1].Path = "deploy/policy.hcl" },
	} {
		source, raw = lifetimeFixture(t, mutate)
		if _, err := Prepare(project.Saferus, LifetimeRecipe, LifetimeMarker, source, raw); err == nil {
			t.Fatal("scope escape")
		}
	}
	for _, body := range []string{LifetimeMarker + "\n" + LifetimeMarker, LifetimeMarker + "\n" + RecipeMarker, strings.Repeat("x", 65537) + LifetimeMarker} {
		if _, ok := SelectRecipe(project.Saferus, body); ok {
			t.Fatal("ambiguous recipe accepted")
		}
	}
	if ValidRecipe(project.Homedir, LifetimeRecipe) || ValidRecipe(project.Saferus, LocaleRecipe) || ValidRecipe("other/repo", LifetimeRecipe) {
		t.Fatal("cross-repository recipe")
	}
}

func TestLifetimeIndependentOracleAndIsolatedWorkload(t *testing.T) {
	rawCases, _ := recipes.ReadFile(recipeDirectory(LifetimeRecipe) + "/cases.json")
	var cases []struct{ Expected string }
	if json.Unmarshal(rawCases, &cases) != nil {
		t.Fatal("cases")
	}
	values := make([]string, len(cases))
	for i, c := range cases {
		values[i] = c.Expected
	}
	oracle, _ := json.Marshal(values)
	before := append([]string{}, values...)
	before[0] = "60 seconds"
	baseline, _ := json.Marshal(before)
	if CompareRecipe(LifetimeRecipe, baseline, oracle, oracle) != nil {
		t.Fatal("valid comparison")
	}
	if CompareRecipe(LifetimeRecipe, oracle, oracle, oracle) == nil || CompareRecipe(LifetimeRecipe, baseline, baseline, oracle) == nil || CompareRecipe(LocaleRecipe, baseline, oracle, oracle) == nil {
		t.Fatal("invalid comparison")
	}
	source, raw := lifetimeFixture(t, nil)
	inputs, err := Prepare(project.Saferus, LifetimeRecipe, LifetimeMarker, source, raw)
	if err != nil {
		t.Fatal(err)
	}
	c := Command{Repository: project.Saferus, CandidateID: "execution-" + strings.Repeat("a", 64), Recipe: LifetimeRecipe, RecipeRevision: RevisionForRecipe(LifetimeRecipe), SourceSHA: source.SourceSHA, CandidateTreeSHA256: inputs.delta.CandidateTreeSHA256, DeltaSHA256: workspacebundle.Digest(raw), ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	w, err := Compile(c, inputs, "example/runner@sha256:"+strings.Repeat("b", 64), executor.DefaultRuntimeClass, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	data := w.ConfigMap["binaryData"].(map[string]any)
	public, _ := base64.StdEncoding.DecodeString(data["cases"].(string))
	if strings.Contains(string(public), "expected") || strings.Contains(string(public), "1 minute") {
		t.Fatal("oracle leaked")
	}
	if len(w.Pods) != 4 {
		t.Fatal("role isolation")
	}
	for role, pod := range w.Pods {
		spec := pod["spec"].(map[string]any)
		if spec["automountServiceAccountToken"] != false || spec["runtimeClassName"] != "ai-sdlc-offline" {
			t.Fatal("unsafe role")
		}
		volume := spec["volumes"].([]any)[0].(map[string]any)["configMap"].(map[string]any)
		for _, v := range volume["items"].([]any) {
			key := v.(map[string]any)["key"].(string)
			if role != "reference" && (key == "reference-cases" || key == "reference-driver") {
				t.Fatal("oracle mounted in candidate")
			}
			if role == "reference" && (key == "candidate" || key == "baseline" || key == "candidate-driver") {
				t.Fatal("untrusted reference")
			}
		}
	}
	c.Repository = project.Homedir
	if _, err := Compile(c, inputs, "example/runner@sha256:"+strings.Repeat("b", 64), executor.DefaultRuntimeClass, time.Now()); err == nil {
		t.Fatal("cross-repo compile")
	}
}
