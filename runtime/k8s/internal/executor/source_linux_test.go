package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func TestWorkerVerifiesOwnTreeBeforeGitBaseline(t *testing.T) {
	b := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: strings.Repeat("a", 40), Files: []workspacebundle.File{{Path: ".gitignore", Mode: 0644, Data: []byte("tracked.txt\n")}, {Path: "tracked.txt", Mode: 0644, Data: []byte("must stay in baseline")}}}
	hash, e := b.TreeDigest()
	if e != nil {
		t.Fatal(e)
	}
	a := Assignment{SourceSHA: b.SourceSHA, TreeSHA256: hash}
	parent := t.TempDir()
	if e = workspacebundle.Import(parent, b); e != nil {
		t.Fatal(e)
	}
	if e = VerifySource(parent, a); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(parent, "repository", "tracked.txt"), []byte("modified"), 0644); e != nil {
		t.Fatal(e)
	}
	if VerifySource(parent, a) == nil {
		t.Fatal("modified source accepted")
	}
}
func TestWorkerRejectsPreexistingGitMetadata(t *testing.T) {
	for _, kind := range []string{"directory", "file", "dangling-link", "external-baseline"} {
		t.Run(kind, func(t *testing.T) {
			b := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: strings.Repeat("a", 40), Files: []workspacebundle.File{}}
			hash, _ := b.TreeDigest()
			a := Assignment{SourceSHA: b.SourceSHA, TreeSHA256: hash}
			parent := t.TempDir()
			if e := workspacebundle.Import(parent, b); e != nil {
				t.Fatal(e)
			}
			p := filepath.Join(parent, "repository", ".git")
			var e error
			switch kind {
			case "directory":
				e = os.Mkdir(p, 0700)
			case "file":
				e = os.WriteFile(p, []byte("gitdir: /other"), 0600)
			case "dangling-link":
				e = os.Symlink("missing", p)
			case "external-baseline":
				e = os.Mkdir(filepath.Join(parent, "git"), 0700)
			}
			if e != nil {
				t.Fatal(e)
			}
			if VerifySource(parent, a) == nil {
				t.Fatal("preexisting Git metadata accepted")
			}
		})
	}
}
