package publication

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Pure local fixture: no database, forge credential, runtime or model call.
func TestRealHomedirPublicationTreeFixture(t *testing.T) {
	path := os.Getenv("AI_SDLC_PUBLICATION_TREE_FIXTURE")
	if path == "" {
		t.Skip("explicit isolated Homedir Git fixture required")
	}
	input, err := os.ReadFile(path)
	var in struct{ SourceFile, CandidateDirectory, ReportFile string }
	if err != nil || len(input) > 16384 || json.Unmarshal(input, &in) != nil {
		t.Fatal("fixture input")
	}
	f, err := os.Open(in.SourceFile)
	if err != nil {
		t.Fatal("source input")
	}
	raw, err := io.ReadAll(io.LimitReader(f, workspacebundle.MaxWireBytes+1))
	f.Close()
	if err != nil {
		t.Fatal("source read")
	}
	source, err := workspacebundle.Decode(raw)
	if err != nil {
		t.Fatal("source bound")
	}
	files := append([]workspacebundle.File(nil), source.Files...)
	for _, name := range []string{verification.LocaleSource, verification.LocaleNewTest} {
		f, err := os.Open(filepath.Join(in.CandidateDirectory, name))
		if err != nil {
			t.Fatal("candidate input")
		}
		data, err := io.ReadAll(io.LimitReader(f, 32769))
		f.Close()
		if err != nil || len(data) == 0 || len(data) > 32768 {
			t.Fatal("candidate bound")
		}
		replaced := false
		for i := range files {
			if files[i].Path == name {
				files[i].Data = data
				replaced = true
			}
		}
		if !replaced {
			files = append(files, workspacebundle.File{Path: name, Mode: 0644, Data: data})
		}
	}
	result := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: source.SourceSHA, Files: files}
	delta, err := candidate.Build(source, result)
	if err != nil {
		t.Fatal("candidate delta")
	}
	raw, err = candidate.Encode(delta)
	if err != nil {
		t.Fatal("canonical delta")
	}
	c := Command{Repository: "os-santiago/homedir", IssueNumber: 3, SourceSHA: source.SourceSHA, CandidateID: "execution-" + strings.Repeat("a", 64), DeltaSHA256: workspacebundle.Digest(raw), CandidateTreeSHA256: delta.CandidateTreeSHA256}
	plan, err := PrepareTree(c, "", source, raw)
	if err != nil {
		t.Fatal("publication plan")
	}
	report, _ := json.Marshal(map[string]any{"sourceSHA": source.SourceSHA, "sourceGitTreeSHA": plan.Request.BaseTree, "candidateGitTreeSHA": plan.ExpectedTreeSHA, "candidateDeltaSHA256": c.DeltaSHA256, "candidateTreeSHA256": c.CandidateTreeSHA256, "changedFiles": len(plan.Request.Tree), "scope": "isolated-publication-tree-fixture", "autonomousE2E": false, "forgeWrites": 0})
	out, err := os.OpenFile(in.ReportFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("report output")
	}
	if _, err = out.Write(report); err != nil {
		t.Fatal("report write")
	}
	if out.Close() != nil {
		t.Fatal("report close")
	}
}
