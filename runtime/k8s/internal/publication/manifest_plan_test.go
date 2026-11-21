package publication

import (
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

func installGeneric(t *testing.T) {
	t.Helper()
	m := project.Manifest{Version: project.ManifestVersion, Repository: "example/service", BaseBranch: "main",
		Verification: &project.Verification{Image: "registry.example/go@sha256:" + strings.Repeat("a", 64),
			Command: []string{"go", "test", "./..."}, TimeoutSeconds: 300, CPU: "2", Memory: "2Gi", EphemeralStorage: "1Gi",
			Network: "none", Result: project.Result{ExitCode: true}, Repeat: 1, TestGlobs: []string{"**/*_test.go"}},
		Publication: project.Publication{Allow: []string{"**/*.go"}, Deny: []string{"go.mod"}, MaxChangedFiles: 2, MaxChangedBytes: 100, MaxFileBytes: 60},
		Merge:       project.Merge{RulesetID: 7, Required: []project.Check{{Context: "ci", AppID: 1}}, StrictChecks: true, ResolveThreads: true}}
	if m.Validate() != nil {
		t.Fatal("fixture manifest invalid")
	}
	project.SetInstalled(&m)
	t.Cleanup(func() { project.SetInstalled(nil) })
}

func TestManifestDrivesThePlanOfANonReferenceRepository(t *testing.T) {
	installGeneric(t)
	p := writerPlan()
	p.Repository = "example/service"
	p.Request.Tree = []TreeEntry{{"pkg/add.go", "100644", "blob", "package pkg"}, {"pkg/add_test.go", "100644", "blob", "package pkg"}}
	if _, err := validatePlan("example/service", p); err != nil {
		t.Fatal("allowed plan refused", err)
	}
	for name, mutate := range map[string]func(*TreePlan){
		"denied path":   func(p *TreePlan) { p.Request.Tree[0].Path = "go.mod" },
		"automation":    func(p *TreePlan) { p.Request.Tree[0].Path = ".github/workflows/x.go" },
		"unlisted path": func(p *TreePlan) { p.Request.Tree[0].Path = "README.md" },
		"too many files": func(p *TreePlan) {
			p.Request.Tree = append(p.Request.Tree, TreeEntry{"pkg/c.go", "100644", "blob", "x"})
		},
		"no files":       func(p *TreePlan) { p.Request.Tree = nil },
		"file too large": func(p *TreePlan) { p.Request.Tree[0].Content = strings.Repeat("x", 61) },
		"total too large": func(p *TreePlan) {
			p.Request.Tree[0].Content, p.Request.Tree[1].Content = strings.Repeat("x", 55), strings.Repeat("y", 55)
		},
		"other repository": func(p *TreePlan) { p.Repository = "example/other" },
	} {
		q := p
		q.Request.Tree = append([]TreeEntry(nil), p.Request.Tree...)
		mutate(&q)
		if _, err := validatePlan("example/service", q); err == nil {
			t.Fatal("plan accepted:", name)
		}
	}
	if _, err := validatePlan("example/other", p); err == nil {
		t.Fatal("unbound repository accepted")
	}
}
