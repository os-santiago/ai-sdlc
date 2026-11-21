package repositorytests

import (
	"strings"
	"testing"

	delta "github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func bundle(files map[string]string) workspacebundle.Bundle {
	b := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: strings.Repeat("a", 40)}
	for _, path := range []string{"go.mod", "textutil/textutil.go", "textutil/textutil_test.go", "textutil/extra_test.go", "README.md"} {
		if data, ok := files[path]; ok {
			b.Files = append(b.Files, workspacebundle.File{Path: path, Mode: 0o644, Data: []byte(data)})
		}
	}
	return b
}

func TestBuildTreesSeparatesTheCandidatesTestsFromItsCode(t *testing.T) {
	base := bundle(map[string]string{"go.mod": "module x\n", "textutil/textutil.go": "old", "textutil/textutil_test.go": "old test", "README.md": "r"})
	result := bundle(map[string]string{"go.mod": "module x\n", "textutil/textutil.go": "fixed", "textutil/textutil_test.go": "new test", "textutil/extra_test.go": "added test"})
	d, err := delta.Build(base, result)
	if err != nil {
		t.Fatal(err)
	}
	trees, err := BuildTrees(base, d, testPod().Verification)
	if err != nil {
		t.Fatal(err)
	}
	contents := func(b workspacebundle.Bundle) map[string]string {
		m := map[string]string{}
		for _, f := range b.Files {
			m[f.Path] = string(f.Data)
		}
		return m
	}
	cross := contents(trees.Cross)
	if cross["textutil/textutil.go"] != "old" || cross["textutil/textutil_test.go"] != "new test" || cross["textutil/extra_test.go"] != "added test" || cross["README.md"] != "r" {
		t.Fatal("cross tree", cross)
	}
	if got := contents(trees.Candidate); got["textutil/textutil.go"] != "fixed" || got["README.md"] != "" {
		t.Fatal("candidate tree", got)
	}
	if baseTree, _ := trees.Baseline.TreeDigest(); baseTree != d.SourceTreeSHA256 {
		t.Fatal("baseline must be the base tree")
	}
	byPath := map[string]Change{}
	for _, c := range trees.Changes {
		byPath[c.Path] = c
	}
	if !byPath["README.md"].Deleted || !byPath["textutil/extra_test.go"].Added || byPath["textutil/textutil.go"].Added || len(byPath) != 4 {
		t.Fatal("changes", trees.Changes)
	}
	for run, want := range map[string]workspacebundle.Bundle{RunBaseline: trees.Baseline, RunCross: trees.Cross, RunCandidate: trees.Candidate} {
		got, ok := trees.Tree(run)
		gotTree, _ := got.TreeDigest()
		wantTree, _ := want.TreeDigest()
		if !ok || gotTree != wantTree {
			t.Fatal("tree for", run)
		}
	}
	for _, run := range []string{"candidate-1", "tests", "reference", ""} {
		if _, ok := trees.Tree(run); ok {
			t.Fatal("tree for", run)
		}
	}
}

func TestBuildTreesRefusesADeltaForAnotherBase(t *testing.T) {
	base := bundle(map[string]string{"go.mod": "module x\n", "textutil/textutil.go": "old"})
	result := bundle(map[string]string{"go.mod": "module x\n", "textutil/textutil.go": "fixed"})
	d, err := delta.Build(base, result)
	if err != nil {
		t.Fatal(err)
	}
	other := bundle(map[string]string{"go.mod": "module y\n", "textutil/textutil.go": "old"})
	if _, err := BuildTrees(other, d, testPod().Verification); err == nil {
		t.Fatal("a delta applied to another base")
	}
	v := testPod().Verification
	v.Network = "default"
	if _, err := BuildTrees(base, d, v); err == nil {
		t.Fatal("unreviewed settings accepted")
	}
}
