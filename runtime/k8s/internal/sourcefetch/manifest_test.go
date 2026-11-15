package sourcefetch

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// The vectors were produced independently with git add/write-tree/ls-tree and
// cat-file, including executable, binary and directory-vs-file sort cases.
func manifestFixture(t *testing.T) (treeManifest, workspacebundle.Bundle) {
	t.Helper()
	raw, err := os.ReadFile("testdata/git-tree.json")
	if err != nil {
		t.Fatal(err)
	}
	var m treeManifest
	if authority.DecodeStrict(raw, &m) != nil {
		t.Fatal("invalid manifest vector")
	}
	raw, err = os.ReadFile("testdata/git-bundle.json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := workspacebundle.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m, b
}
func TestGitObjectManifestRejectsExportOmissionsAndTransformations(t *testing.T) {
	m, b := manifestFixture(t)
	if m.SHA != "9eb0703bb667ab2cb44a45e1d93d5e3489c816ab" || m.verify(b) != nil {
		t.Fatal("independent Git object vector failed")
	}
	for name, mutate := range map[string]func(*workspacebundle.Bundle){
		"export-ignore":       func(b *workspacebundle.Bundle) { b.Files = b.Files[1:] },
		"export-subst-or-lfs": func(b *workspacebundle.Bundle) { b.Files[0].Data[0] ^= 1 },
		"mode":                func(b *workspacebundle.Bundle) { b.Files[0].Mode = 0755 },
		"unexpected-file": func(b *workspacebundle.Bundle) {
			b.Files = append(b.Files, workspacebundle.File{Path: "extra", Mode: 0644, Data: []byte{}})
		},
		"renamed": func(b *workspacebundle.Bundle) { b.Files[0].Path = "other-name" },
	} {
		t.Run(name, func(t *testing.T) {
			m, b := manifestFixture(t)
			mutate(&b)
			if m.verify(b) == nil {
				t.Fatal("transformed source accepted")
			}
		})
	}
}

func TestGitTreeManifestRejectsUnsupportedOrInconsistentEntries(t *testing.T) {
	for name, mutate := range map[string]func(*treeManifest){
		"wrong-root": func(m *treeManifest) { m.SHA = strings.Repeat("a", 40) },
		"duplicate":  func(m *treeManifest) { m.Tree = append(m.Tree, m.Tree[0]) },
		"missing-parent": func(m *treeManifest) {
			for i, e := range m.Tree {
				if e.Type == "tree" {
					m.Tree = append(m.Tree[:i], m.Tree[i+1:]...)
					return
				}
			}
		},
		"wrong-directory-hash": func(m *treeManifest) {
			for i, e := range m.Tree {
				if e.Type == "tree" {
					m.Tree[i].SHA = strings.Repeat("a", 40)
					return
				}
			}
		},
		"symlink":                  func(m *treeManifest) { m.Tree[0].Mode = "120000" },
		"submodule":                func(m *treeManifest) { m.Tree[0].Mode = "160000"; m.Tree[0].Type = "commit" },
		"traversal":                func(m *treeManifest) { m.Tree[0].Path = "../escape" },
		"null-size":                func(m *treeManifest) { m.Tree[0].Size = nil },
		"oversized-file":           func(m *treeManifest) { v := int64(workspacebundle.MaxFileBytes + 1); m.Tree[0].Size = &v },
		"truncated":                func(m *treeManifest) { v := true; m.Truncated = &v },
		"missing-truncation-proof": func(m *treeManifest) { m.Truncated = nil },
	} {
		t.Run(name, func(t *testing.T) {
			m, b := manifestFixture(t)
			mutate(&m)
			if m.verify(b) == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}

func TestActualPinnedGitTreeManifest(t *testing.T) {
	file := os.Getenv("AI_SDLC_TEST_SOURCE_TREE")
	archive := os.Getenv("AI_SDLC_TEST_SOURCE_ARCHIVE")
	if file == "" || archive == "" {
		t.Skip("actual downloaded tree and archive required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var m treeManifest
	if authority.DecodeStrict(raw, &m) != nil {
		t.Fatal("invalid actual Git tree")
	}
	raw, err = os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	b, err := DecodeArchive(raw, testSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.verify(b); err != nil {
		t.Fatal(err)
	}
	// Re-encoding a manifest cannot erase a malformed/duplicate-key input;
	// decoding occurs strictly before the typed verifier.
	encoded, _ := json.Marshal(m)
	if len(encoded) > MaxManifestBytes {
		t.Fatal("actual fixture exceeds contract")
	}
}
