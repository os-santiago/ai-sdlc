package verification

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func TestRealVerificationRecipeFixture(t *testing.T) {
	path := os.Getenv("AI_SDLC_VERIFICATION_FIXTURE")
	if path == "" {
		t.Skip("explicit isolated recipe fixture required")
	}
	var in struct {
		SourceFile, PlanFile, ResultsFile, ReportFile, Image string
		Changes                                              map[string][]byte
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 128*1024 || json.Unmarshal(raw, &in) != nil {
		t.Fatal("private fixture input")
	}
	raw, err = os.ReadFile(in.SourceFile)
	if err != nil {
		t.Fatal("source fixture")
	}
	source, err := workspacebundle.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	result := source
	result.Files = append([]workspacebundle.File{}, source.Files...)
	for path, data := range in.Changes {
		found := false
		for i, f := range result.Files {
			if f.Path == path {
				result.Files[i].Data = data
				found = true
				break
			}
		}
		if !found {
			result.Files = append(result.Files, workspacebundle.File{Path: path, Mode: 0644, Data: data})
		}
	}
	delta, err := candidate.Build(source, result)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = candidate.Encode(delta)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare("os-santiago/homedir", LocaleRecipe, RecipeMarker, source, raw)
	if err != nil {
		t.Fatal(err)
	}
	c := Command{Repository: "os-santiago/homedir", CandidateID: "execution-" + workspacebundle.Digest(raw), Recipe: LocaleRecipe, RecipeRevision: RecipeRevision(), SourceSHA: source.SourceSHA, CandidateTreeSHA256: delta.CandidateTreeSHA256, DeltaSHA256: workspacebundle.Digest(raw), ExpiresAt: time.Now().Add(3 * time.Minute).UnixMilli()}
	w, err := Compile(c, prepared, in.Image, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(w)
	if os.WriteFile(in.PlanFile, raw, 0600) != nil {
		t.Fatal("fixture plan")
	}
	deadline := time.Now().Add(100 * time.Second)
	for {
		raw, err = os.ReadFile(in.ResultsFile)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture results deadline")
		}
		time.Sleep(50 * time.Millisecond)
	}
	var results struct{ Baseline, Candidate, Reference []byte }
	if json.Unmarshal(raw, &results) != nil || Compare(results.Baseline, results.Candidate, results.Reference) != nil {
		t.Fatal("independent behavior comparison failed")
	}
	report, _ := json.Marshal(map[string]any{"scope": "isolated-independent-recipe-fixture", "syntheticCommand": true, "realKubernetes": true, "offlineRuntime": true, "recipe": LocaleRecipe, "recipeRevision": RecipeRevision(), "sourceSHA": source.SourceSHA, "candidateTreeSHA256": delta.CandidateTreeSHA256, "deltaSHA256": c.DeltaSHA256, "baselineDefectReproduced": true, "independentBehaviorCompared": true, "cases": 15, "publicationAuthorized": false, "autonomousE2E": false})
	if os.WriteFile(in.ReportFile, report, 0600) != nil {
		t.Fatal("fixture report")
	}
}
