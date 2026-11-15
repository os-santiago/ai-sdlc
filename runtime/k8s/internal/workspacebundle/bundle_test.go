package workspacebundle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func fixture() Bundle {
	return Bundle{Version, strings.Repeat("a", 40), []File{{"README.md", 0644, []byte("hello\n")}, {"bin/check", 0755, []byte("#!/bin/sh\nexit 0\n")}, {"nested/日本語.txt", 0644, []byte("áé🦀\r\n")}, {"zero", 0644, []byte{}}}}
}
func TestRoundTripExactContentAndExecutableMode(t *testing.T) {
	b := fixture()
	raw, e := Encode(b)
	if e != nil {
		t.Fatal(e)
	}
	decoded, e := ReadVerified(bytes.NewReader(raw), Digest(raw), b.SourceSHA)
	if e != nil || !reflect.DeepEqual(b, decoded) {
		t.Fatalf("decode %v", e)
	}
	parent := t.TempDir()
	if e = Import(parent, decoded); e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir(filepath.Join(parent, "repository", ".git"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(parent, "repository", ".git", "config"), []byte("must not export"), 0600); e != nil {
		t.Fatal(e)
	}
	candidate, e := Export(parent, b.SourceSHA)
	if e != nil || !reflect.DeepEqual(candidate, b) {
		t.Fatalf("export differs: %v %#v", e, candidate)
	}
	if e = Import(parent, b); e == nil {
		t.Fatal("overwrote existing repository")
	}
	for _, f := range b.Files {
		info, e := os.Stat(filepath.Join(parent, "repository", f.Path))
		if e != nil || uint32(info.Mode().Perm()) != f.Mode {
			t.Fatalf("mode %s %v", f.Path, e)
		}
	}
}
func TestDecodeRejectsAmbiguityAndNull(t *testing.T) {
	raw, _ := Encode(fixture())
	cases := []string{
		strings.Replace(string(raw), `"version":`, `"Version":`, 1),
		strings.Replace(string(raw), `"sourceSHA":`, `"sourceSHA":"`+strings.Repeat("b", 40)+`","sourceSHA":`, 1),
		strings.Replace(string(raw), `"path":`, `"Path":`, 1),
		strings.Replace(string(raw), `"path":"README.md"`, `"path":"README.md","path":"other"`, 1),
		strings.Replace(string(raw), `"mode":420`, `"mode":420.0`, 1),
		strings.Replace(string(raw), `"data":"aGVsbG8K"`, `"data":null`, 1),
		strings.Replace(string(raw), `"data":"aGVsbG8K"`, `"data":"aGVsbG8K\n"`, 1),
		strings.Replace(string(raw), `"data":"aGVsbG8K"`, `"data":"YQ==="`, 1),
		strings.Replace(string(raw), `"path":"README.md"`, `"path":"\ud800"`, 1),
		strings.Replace(string(raw), `"path":"README.md"`, `"path":"`+string([]byte{0xff})+`"`, 1),
		string(raw) + `{}`, `null`, `[]`, `{"version":"` + Version + `","sourceSHA":"` + fixture().SourceSHA + `","files":null}`,
	}
	for i, raw := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if _, e := Decode([]byte(raw)); e == nil {
				t.Fatal("malformed accepted")
			}
		})
	}
}
func TestRejectsPathsAndTreeConflictsBeforeWriting(t *testing.T) {
	paths := []string{"", ".", "..", "../outside", "/outside", "a//b", "./a", "a/../b", "a/", "a\\b", ".git/config", "a/.GIT/config", "a\x00b", "a\nb", "a\tb", strings.Repeat("a", MaxPathBytes+1), strings.Repeat("a/", MaxDepth) + "b"}
	for _, name := range paths {
		b := fixture()
		b.Files[0].Path = name
		parent := t.TempDir()
		if Import(parent, b) == nil {
			t.Fatalf("accepted %q", name)
		}
		if _, e := os.Stat(filepath.Join(parent, "repository")); !os.IsNotExist(e) {
			t.Fatalf("wrote before validation for %q", name)
		}
	}
	for _, files := range [][]File{{{"a", 0644, []byte{}}, {"a", 0644, []byte{}}}, {{"a", 0644, []byte{}}, {"a/b", 0644, []byte{}}}, {{"a/b", 0644, []byte{}}, {"a", 0644, []byte{}}}} {
		b := fixture()
		b.Files = files
		parent := t.TempDir()
		if Import(parent, b) == nil {
			t.Fatal("accepted conflicting tree")
		}
		if _, e := os.Stat(filepath.Join(parent, "repository")); !os.IsNotExist(e) {
			t.Fatal("created conflicting tree")
		}
	}
}
func TestLimitsAndIdentity(t *testing.T) {
	b := fixture()
	raw, _ := Encode(b)
	if _, e := ReadVerified(bytes.NewReader(raw), strings.Repeat("0", 64), b.SourceSHA); e == nil {
		t.Fatal("bad digest")
	}
	if _, e := ReadVerified(bytes.NewReader(raw), Digest(raw), strings.Repeat("b", 40)); e == nil {
		t.Fatal("wrong source")
	}
	b.Files = []File{{"big", 0644, bytes.Repeat([]byte("x"), MaxFileBytes+1)}}
	if _, e := Encode(b); e == nil {
		t.Fatal("per-file limit")
	}
	raw, _ = json.Marshal(b)
	if _, e := Decode(raw); e == nil {
		t.Fatal("decoder per-file limit")
	}
	b.Files = make([]File, MaxFiles+1)
	for i := range b.Files {
		b.Files[i] = File{fmt.Sprint(i), 0644, []byte{}}
	}
	raw, _ = json.Marshal(b)
	if _, e := Decode(raw); e == nil {
		t.Fatal("count limit")
	}
	b.Files = make([]File, 5)
	for i := range b.Files {
		b.Files[i] = File{fmt.Sprint(i), 0644, bytes.Repeat([]byte("x"), MaxFileBytes)}
	}
	raw, _ = json.Marshal(b)
	if _, e := Decode(raw); e == nil {
		t.Fatal("aggregate limit")
	}
	if _, e := ReadVerified(ioRepeatReader{}, strings.Repeat("a", 64), b.SourceSHA); e == nil {
		t.Fatal("wire limit")
	}
	for _, mode := range []uint32{0, 0600, 0777, 04755, 0120644} {
		b = fixture()
		b.Files[0].Mode = mode
		if b.Validate() == nil {
			t.Fatalf("mode %o", mode)
		}
	}
}

type ioRepeatReader struct{}

func (ioRepeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
func TestExportRejectsHostileNodes(t *testing.T) {
	for _, kind := range []string{"symlink-out", "symlink-in", "hardlink", "fifo", "large", "special-mode", "nested-git", "directory-link"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			b := fixture()
			if e := Import(parent, b); e != nil {
				t.Fatal(e)
			}
			root := filepath.Join(parent, "repository")
			outside := filepath.Join(parent, "private")
			if e := os.WriteFile(outside, []byte("not in workspace"), 0600); e != nil {
				t.Fatal(e)
			}
			var e error
			switch kind {
			case "symlink-out":
				e = os.Symlink(outside, filepath.Join(root, "malicious"))
			case "symlink-in":
				e = os.Symlink("README.md", filepath.Join(root, "malicious"))
			case "hardlink":
				e = os.Link(outside, filepath.Join(root, "malicious"))
			case "fifo":
				e = unix.Mkfifo(filepath.Join(root, "malicious"), 0600)
			case "large":
				e = os.WriteFile(filepath.Join(root, "malicious"), make([]byte, MaxFileBytes+1), 0644)
			case "special-mode":
				e = unix.Chmod(filepath.Join(root, "README.md"), 04755)
			case "nested-git":
				e = os.Mkdir(filepath.Join(root, "nested", ".git"), 0700)
			case "directory-link":
				e = os.Symlink("nested", filepath.Join(root, "malicious"))
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, e = Export(parent, b.SourceSHA); e == nil {
				t.Fatalf("export accepted %s", kind)
			}
			actual, e := os.ReadFile(outside)
			if e != nil || string(actual) != "not in workspace" {
				t.Fatal("outside content changed")
			}
		})
	}
}
func TestExportBoundsEmptyDirectoriesAndDepth(t *testing.T) {
	parent := t.TempDir()
	b := fixture()
	b.Files = []File{}
	if e := Import(parent, b); e != nil {
		t.Fatal(e)
	}
	root := filepath.Join(parent, "repository")
	for i := 0; i <= MaxNodes; i++ {
		if e := os.Mkdir(filepath.Join(root, fmt.Sprint(i)), 0755); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := Export(parent, b.SourceSHA); e == nil {
		t.Fatal("unbounded empty directories")
	}
	parent = t.TempDir()
	if e := Import(parent, b); e != nil {
		t.Fatal(e)
	}
	root = filepath.Join(parent, "repository")
	for range MaxDepth + 1 {
		root = filepath.Join(root, "d")
		if e := os.Mkdir(root, 0755); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := Export(parent, b.SourceSHA); e == nil {
		t.Fatal("unbounded depth")
	}
}
func TestImportRefusesExistingAndSymlinkParents(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if e := os.Symlink(real, link); e != nil {
		t.Fatal(e)
	}
	if Import(link, fixture()) == nil {
		t.Fatal("symlink parent")
	}
	if e := os.Symlink(real, filepath.Join(real, "repository")); e != nil {
		t.Fatal(e)
	}
	if Import(real, fixture()) == nil {
		t.Fatal("existing symlink workspace")
	}
	if _, e := Export(real, fixture().SourceSHA); e == nil {
		t.Fatal("export symlink root")
	}
}
func TestFileStateIgnoresAccessTimeOnly(t *testing.T) {
	a := unix.Stat_t{Ino: 1, Size: 3, Nlink: 1, Mode: unix.S_IFREG | 0644}
	b := a
	b.Atim.Sec++
	if !sameFileState(a, b) {
		t.Fatal("read access time treated as mutation")
	}
	b.Ctim.Sec++
	if sameFileState(a, b) {
		t.Fatal("changed metadata accepted")
	}
}

func TestImportBoundsImplicitDirectoryNodesBeforeWriting(t *testing.T) {
	b := fixture()
	b.Files = []File{}
	for i := 0; i < 300; i++ {
		b.Files = append(b.Files, File{fmt.Sprintf("%d/", i) + strings.Repeat("d/", 29) + "file", 0644, []byte{}})
	}
	parent := t.TempDir()
	if b.Validate() == nil || Import(parent, b) == nil {
		t.Fatal("implicit directory node budget ignored")
	}
	if _, e := os.Stat(filepath.Join(parent, "repository")); !os.IsNotExist(e) {
		t.Fatal("workspace created before node validation")
	}
	raw, e := json.Marshal(b)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Decode(raw); e == nil {
		t.Fatal("decoder accepted implicit node overflow")
	}
}
func TestImportRejectsLongSegmentsBeforeWriting(t *testing.T) {
	for _, name := range []string{strings.Repeat("x", 256), strings.Repeat("x", 256) + "/file", strings.Repeat("á", 128), strings.Repeat("á", 128) + "/file"} {
		b := fixture()
		b.Files[0].Path = name
		parent := t.TempDir()
		if b.Validate() == nil || Import(parent, b) == nil {
			t.Fatal("oversized segment accepted")
		}
		if _, e := os.Stat(filepath.Join(parent, "repository")); !os.IsNotExist(e) {
			t.Fatal("workspace created before segment validation")
		}
	}
	b := fixture()
	b.Files[0].Path = strings.Repeat("x", 255)
	if b.Validate() != nil {
		t.Fatal("valid boundary rejected")
	}
}

func TestExportUsesGitOwnerExecutableBit(t *testing.T) {
	for _, mode := range []os.FileMode{0600, 0611, 0700, 0711} {
		parent := t.TempDir()
		b := fixture()
		if e := Import(parent, b); e != nil {
			t.Fatal(e)
		}
		if e := os.Chmod(filepath.Join(parent, "repository", "README.md"), mode); e != nil {
			t.Fatal(e)
		}
		out, e := Export(parent, b.SourceSHA)
		if e != nil {
			t.Fatal(e)
		}
		want := uint32(0644)
		if mode&0100 != 0 {
			want = 0755
		}
		if out.Files[0].Mode != want {
			t.Fatalf("mode %o produced %o, want %o", mode, out.Files[0].Mode, want)
		}
	}
}
