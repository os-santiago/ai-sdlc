package podtransport

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// The driver owns a disposable namespace and deny policy. The actual transport
// performs all workload creation, result reads and UID-based deletion.
func TestRealVerificationTransportFixture(t *testing.T) {
	path := os.Getenv("AI_SDLC_VERIFICATION_TRANSPORT_FIXTURE")
	if path == "" {
		t.Skip("explicit isolated kind fixture required")
	}
	var in struct {
		Config                        Config
		SourceFile, ReportFile, Image string
		Changes                       map[string][]byte
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 256*1024 || json.Unmarshal(raw, &in) != nil {
		t.Fatal("private fixture input")
	}
	raw, err = os.ReadFile(in.SourceFile)
	if err != nil {
		t.Fatal("fixture source")
	}
	source, err := workspacebundle.Decode(raw)
	if err != nil {
		t.Fatal("source decode")
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
	d, err := candidate.Build(source, result)
	if err != nil {
		t.Fatal("candidate build")
	}
	raw, err = candidate.Encode(d)
	if err != nil {
		t.Fatal("candidate encode")
	}
	inputs, err := verification.Prepare("os-santiago/homedir", verification.LocaleRecipe, verification.RecipeMarker, source, raw)
	if err != nil {
		t.Fatal("recipe prepare")
	}
	c := verification.Command{CandidateID: "execution-" + workspacebundle.Digest(raw), Recipe: verification.LocaleRecipe, RecipeRevision: verification.RecipeRevision(), SourceSHA: source.SourceSHA, CandidateTreeSHA256: d.CandidateTreeSHA256, DeltaSHA256: workspacebundle.Digest(raw), ExpiresAt: time.Now().Add(3 * time.Minute).UnixMilli()}
	client, err := NewClient(in.Config)
	if err != nil {
		t.Fatal("fixture client")
	}
	v, err := PlanVerification(client, c, inputs, in.Image)
	if err != nil {
		t.Fatal("transport plan")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	configUID, err := v.CreateConfig(ctx)
	if err != nil {
		t.Fatal("transport config")
	}
	uids := map[string]string{}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		for kind, uid := range uids {
			if v.DeletePod(cleanup, kind, uid) != nil {
				t.Error("UID cleanup Pod")
			}
		}
		if v.DeleteConfig(cleanup, configUID) != nil {
			t.Error("UID cleanup config")
		}
	}()
	for _, kind := range verificationRoles {
		uid, e := v.CreatePod(ctx, kind, configUID)
		if e != nil {
			t.Fatal("transport Pod", kind)
		}
		uids[kind] = uid
	}
	output, err := v.ReadResults(ctx, uids)
	if err != nil {
		t.Fatal("transport results")
	}
	if verification.Compare(output.Baseline, output.Candidate, output.Reference) != nil {
		t.Fatal("independent behavior comparison")
	}
	report, _ := json.Marshal(map[string]any{"scope": "isolated-verification-transport-fixture", "syntheticCommand": true, "actualTransport": true, "separatePods": 4, "sourceSHA": c.SourceSHA, "deltaSHA256": c.DeltaSHA256, "recipeRevision": c.RecipeRevision, "independentBehaviorCompared": true, "autonomousE2E": false})
	if os.WriteFile(in.ReportFile, report, 0600) != nil {
		t.Fatal("fixture report")
	}
}
