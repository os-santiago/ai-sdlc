package publication

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func TestGitTreeIdentityMatchesIndependentGitVector(t *testing.T) {
	raw, err := os.ReadFile("../sourcefetch/testdata/git-bundle.json")
	if err != nil {
		t.Fatal(err)
	}
	var bundle workspacebundle.Bundle
	if json.Unmarshal(raw, &bundle) != nil {
		t.Fatal("bundle vector")
	}
	tree, err := os.ReadFile("../sourcefetch/testdata/git-tree.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected struct {
		SHA string `json:"sha"`
	}
	if json.Unmarshal(tree, &expected) != nil {
		t.Fatal("tree vector")
	}
	got, err := GitTreeSHA(bundle)
	if err != nil || got != expected.SHA {
		t.Fatal("Git tree mismatch", got, expected.SHA, err)
	}
	bundle.Files[0].Data = append(bundle.Files[0].Data, '!')
	changed, err := GitTreeSHA(bundle)
	if err != nil || changed == got {
		t.Fatal("changed bytes lost")
	}
}

func TestTreePlanPreservesCompleteSourceAndFixedDestination(t *testing.T) {
	_, c, _, _ := fixture(t)
	source := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: c.SourceSHA, Files: []workspacebundle.File{
		{Path: ".gitattributes", Mode: 0644, Data: []byte("* filter=untrusted\n")},
		{Path: verification.LocaleSource, Mode: 0644, Data: []byte("old source")},
		{Path: verification.LocaleExistingTest, Mode: 0644, Data: []byte("existing test")},
	}}
	result := source
	result.Files = append([]workspacebundle.File(nil), source.Files...)
	result.Files[1].Data = []byte("new source")
	result.Files = append(result.Files, workspacebundle.File{Path: verification.LocaleNewTest, Mode: 0644, Data: []byte("new test")})
	d, err := candidate.Build(source, result)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := candidate.Encode(d)
	if err != nil {
		t.Fatal(err)
	}
	c.DeltaSHA256 = workspacebundle.Digest(raw)
	c.CandidateTreeSHA256 = d.CandidateTreeSHA256
	plan, err := PrepareTree(c, source, raw)
	if err != nil || len(plan.Request.Tree) != 2 || plan.Request.BaseTree == plan.ExpectedTreeSHA {
		t.Fatal("tree plan", err)
	}
	if !strings.HasPrefix(plan.Branch, "ai-sdlc/issue-3-") || !strings.Contains(plan.CommitMessage, "[skip release]") {
		t.Fatal("unbounded destination or release scope")
	}
	if plan.CommitIdentity != project.DefaultCommitIdentity {
		t.Fatal("a manifest without commitIdentity must plan the default identity", plan.CommitIdentity)
	}
	// An installed manifest's identity is resolved into the plan, so the plan
	// digest the claim records binds it.
	configured, _ := project.Builtin(project.Homedir)
	configured.Publication.CommitIdentity = &project.CommitIdentity{Name: "Example Bot", Email: "bot@example.org"}
	project.SetInstalled(&configured)
	reconfigured, err := PrepareTree(c, source, raw)
	project.SetInstalled(nil)
	if err != nil || reconfigured.CommitIdentity != *configured.Publication.CommitIdentity {
		t.Fatal("configured identity not planned", err, reconfigured.CommitIdentity)
	}
	before, _ := json.Marshal(plan)
	after, _ := json.Marshal(reconfigured)
	if authority.Digest(before) == authority.Digest(after) {
		t.Fatal("identity not bound into the plan digest")
	}
	for _, entry := range plan.Request.Tree {
		if entry.Mode != "100644" || entry.Type != "blob" || (entry.Path != verification.LocaleSource && entry.Path != verification.LocaleNewTest) {
			t.Fatal("unexpected publication file")
		}
	}
	changed := c
	changed.DeltaSHA256 = strings.Repeat("f", 64)
	if _, err = PrepareTree(changed, source, raw); err == nil {
		t.Fatal("delta substituted")
	}
	changed = c
	changed.CandidateTreeSHA256 = strings.Repeat("f", 64)
	if _, err = PrepareTree(changed, source, raw); err == nil {
		t.Fatal("tree substituted")
	}
	result.Files[0].Data = []byte("unsafe workflow change")
	d, err = candidate.Build(source, result)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = candidate.Encode(d)
	c.DeltaSHA256 = workspacebundle.Digest(raw)
	c.CandidateTreeSHA256 = d.CandidateTreeSHA256
	if _, err = PrepareTree(c, source, raw); err == nil {
		t.Fatal("out-of-recipe write accepted")
	}
}
