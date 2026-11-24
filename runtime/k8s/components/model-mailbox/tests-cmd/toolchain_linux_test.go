package main

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// elf64 returns a minimal little-endian ELF64 executable header followed by
// program headers of the given types.
func elf64(types ...uint32) []byte {
	b := make([]byte, 64+56*len(types))
	copy(b, "\x7fELF")
	b[4], b[5], b[6] = 2, 1, 1
	binary.LittleEndian.PutUint16(b[16:], 2) // ET_EXEC
	binary.LittleEndian.PutUint64(b[32:], 64)
	binary.LittleEndian.PutUint16(b[54:], 56)
	binary.LittleEndian.PutUint16(b[56:], uint16(len(types)))
	for i, t := range types {
		binary.LittleEndian.PutUint32(b[64+56*i:], t)
	}
	return b
}

// elf32BE returns a minimal big-endian ELF32 header with program headers.
func elf32BE(types ...uint32) []byte {
	b := make([]byte, 52+32*len(types))
	copy(b, "\x7fELF")
	b[4], b[5], b[6] = 1, 2, 1
	binary.BigEndian.PutUint32(b[28:], 52)
	binary.BigEndian.PutUint16(b[42:], 32)
	binary.BigEndian.PutUint16(b[44:], uint16(len(types)))
	for i, t := range types {
		binary.BigEndian.PutUint32(b[52+32*i:], t)
	}
	return b
}

const ptLoad, ptInterp = 1, 3

type tree map[string]struct {
	mode os.FileMode
	data []byte
	link string
}

// toolchainFixture writes a source root and returns it and a destination
// beside it that does not exist yet.
func toolchainFixture(t *testing.T, files tree) (string, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "go")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, f := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		var err error
		switch {
		case f.link != "":
			err = os.Symlink(f.link, p)
		case f.mode.IsDir():
			err = os.Mkdir(p, f.mode.Perm())
		default:
			err = os.WriteFile(p, f.data, 0o600)
		}
		if err == nil && f.link == "" {
			// Chmod after creation, so the umask cannot drop special bits.
			err = os.Chmod(p, f.mode)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "volume")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(out, "toolchain")
	// The copy is read-only; restore write permission so a non-root test
	// run can remove it (cleanups run before t.TempDir's).
	t.Cleanup(func() { removeStaging(dest) })
	return root, dest
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

func TestCopyToolchainCopiesANormalizedReadOnlyTree(t *testing.T) {
	root, dest := toolchainFixture(t, tree{
		"bin/go":                      {mode: 0o755, data: elf64(ptLoad)},
		"bin/setuid":                  {mode: os.ModeSetuid | os.ModeSetgid | 0o755, data: []byte("#!/bin/sh\n")},
		"pkg/tool/linux_amd64/vet":    {mode: 0o755, data: elf32BE(ptLoad)},
		"src/fmt/print.go":            {mode: 0o664, data: []byte("package fmt\n")},
		"src/debug/testdata/dyn.elf":  {mode: 0o644, data: elf64(ptLoad, ptInterp)},
		"src/empty":                   {mode: 0o644},
		"shared":                      {mode: os.ModeDir | os.ModeSticky | 0o777},
		"lib/libexec":                 {link: "../bin"},
		"lib/time/zoneinfo.zip":       {link: "../../src/fmt/print.go"},
		"misc/same-dir":               {link: "print.go"},
		"src/fmt/nested/deep/up-down": {link: "../../../debug/testdata"},
	})
	if mode(t, filepath.Join(root, "bin/setuid"))&(os.ModeSetuid|os.ModeSetgid) != os.ModeSetuid|os.ModeSetgid ||
		mode(t, filepath.Join(root, "shared"))&os.ModeSticky == 0 {
		t.Fatal("fixture lacks special bits")
	}
	if err := copyTree(root, dest, defaultToolchainLimits); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{
		".":                          os.ModeDir | 0o555,
		"bin":                        os.ModeDir | 0o555,
		"bin/go":                     0o555,
		"bin/setuid":                 0o555,
		"pkg/tool/linux_amd64/vet":   0o555,
		"src/fmt/print.go":           0o444,
		"src/debug/testdata/dyn.elf": 0o444,
		"src/empty":                  0o444,
		"shared":                     os.ModeDir | 0o555,
	} {
		if got := mode(t, filepath.Join(dest, name)); got != want {
			t.Fatalf("%s: mode %v, want %v", name, got, want)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dest, "lib/time/zoneinfo.zip")); err != nil || string(raw) != "package fmt\n" {
		t.Fatal("internal link not copied", err)
	}
	if target, err := os.Readlink(filepath.Join(dest, "lib/libexec")); err != nil || target != "../bin" {
		t.Fatal("link target changed", target, err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(dest), ".toolchain.partial")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staging directory left behind")
	}
}

func TestCopyToolchainRefusesLinksThatMayLeaveTheRoot(t *testing.T) {
	for _, c := range []struct{ name, target string }{
		{"escape", "../outside"},
		{"absolute", "/etc/passwd"},
		{"absolute inside", "/usr/local/go/bin/go"},
		{"nested escape", "a/../../outside"},
		{"inner parent", "bin/../src"},
		{"deep escape", "../../../../outside"},
		{"dot", "./bin"},
		{"self", "."},
		{"ancestor", ".."},
	} {
		depth := "sub/"
		if c.name == "escape" || c.name == "ancestor" {
			depth = "" // at the root, even one ".." leaves it
		}
		root, dest := toolchainFixture(t, tree{"bin/go": {mode: 0o755, data: elf64(ptLoad)}, depth + "link": {link: c.target}})
		err := copyTree(root, dest, defaultToolchainLimits)
		assertRefused(t, c.name, err, exitToolchainLink, dest, root)
	}
}

func TestSafeLinkTargets(t *testing.T) {
	for _, c := range []struct {
		target string
		depth  int
		safe   bool
	}{
		{"go", 0, true}, {"a/b", 0, true}, {"../a", 1, true}, {"../../a/b", 2, true},
		{"../a", 0, false}, {"../../a", 1, false}, {"a/../b", 3, false}, {"../a/../b", 2, false},
		{"/a", 3, false}, {"", 1, false}, {"a//b", 1, false}, {"a/", 1, false}, {"..", 2, false},
		{"a\x00b", 1, false}, {strings.Repeat("a", 1025), 0, false},
	} {
		if safeLink(c.target, c.depth) != c.safe {
			t.Fatalf("safeLink(%q, %d) != %v", c.target, c.depth, c.safe)
		}
	}
}

func TestCopyToolchainRefusesDynamicallyLinkedExecutables(t *testing.T) {
	for name, f := range map[string][]byte{
		"elf64 interp":   elf64(ptLoad, ptInterp),
		"elf32be interp": elf32BE(ptInterp),
	} {
		root, dest := toolchainFixture(t, tree{"bin/tool": {mode: 0o755, data: f}})
		assertRefused(t, name, copyTree(root, dest, defaultToolchainLimits), exitToolchainDynamic, dest, root)
	}
	truncated := elf64(ptLoad, ptInterp)[:80]
	root, dest := toolchainFixture(t, tree{"bin/tool": {mode: 0o700, data: truncated}})
	assertRefused(t, "truncated program headers", copyTree(root, dest, defaultToolchainLimits), exitToolchainSource, dest, root)
	header := elf64()[:10]
	root, dest = toolchainFixture(t, tree{"bin/tool": {mode: 0o700, data: header}})
	assertRefused(t, "truncated header", copyTree(root, dest, defaultToolchainLimits), exitToolchainSource, dest, root)
}

func TestCopyToolchainEnforcesItsBounds(t *testing.T) {
	files := tree{"a": {mode: 0o644, data: []byte("1234")}, "b/c": {mode: 0o644, data: []byte("5678")}}
	// Entries: a, b and b/c.
	for name, c := range map[string]struct {
		limits toolchainLimits
		ok     bool
	}{
		"files at cap":  {toolchainLimits{files: 3, bytes: 8, depth: 1}, true},
		"files over":    {toolchainLimits{files: 2, bytes: 8, depth: 1}, false},
		"bytes over":    {toolchainLimits{files: 3, bytes: 7, depth: 1}, false},
		"depth over":    {toolchainLimits{files: 3, bytes: 8, depth: 0}, false},
		"defaults hold": {defaultToolchainLimits, true},
	} {
		root, dest := toolchainFixture(t, files)
		err := copyTree(root, dest, c.limits)
		if c.ok {
			if err != nil {
				t.Fatal(name, err)
			}
			continue
		}
		assertRefused(t, name, err, exitToolchainLimit, dest, root)
	}
	if defaultToolchainLimits.files != 50_000 || defaultToolchainLimits.bytes != 1<<30 {
		t.Fatal("default bounds changed")
	}
}

func TestCopyToolchainRefusesSpecialFilesAndAnExistingDestination(t *testing.T) {
	root, dest := toolchainFixture(t, tree{"bin/go": {mode: 0o755, data: elf64(ptLoad)}})
	if err := syscall.Mkfifo(filepath.Join(root, "bin/fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertRefused(t, "fifo", copyTree(root, dest, defaultToolchainLimits), exitToolchainSource, dest, root)

	root, dest = toolchainFixture(t, tree{"bin/go": {mode: 0o755, data: elf64(ptLoad)}})
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(root, dest, defaultToolchainLimits); !isRefusal(err, exitToolchainDestination) {
		t.Fatal("existing destination accepted", err)
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 0 {
		t.Fatal("existing destination was written")
	}

	link := filepath.Join(t.TempDir(), "go")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(link, dest+"2", defaultToolchainLimits); !isRefusal(err, exitToolchainSource) {
		t.Fatal("symbolic link root followed", err)
	}
}

func TestCopyToolchainValidatesItsArguments(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"/usr/local/go"},
		{"usr/local/go", "/toolchain/go"},
		{"/", "/toolchain/go"},
		{"/tmp/go", "/toolchain/go"},
		{"/usr/local/go", "toolchain"},
		{"/usr/local/go", "/toolchain/../go"},
		{"/usr/local/go", "/"},
		{"/usr/local/go", "/usr/local/go/copy"},
		{"/usr/local/go", "/usr/local"},
		{"/usr/local/go", "/usr/local/go"},
		{"/usr/local/go", "/toolchain/go", "extra"},
	} {
		if err := copyToolchain(args, defaultToolchainLimits); !isRefusal(err, exitToolchainUsage) {
			t.Fatal("accepted arguments", args, err)
		}
	}
}

func isRefusal(err error, code int) bool {
	var refusal *toolchainError
	return errors.As(err, &refusal) && refusal.code == code
}

// assertRefused checks the exit code, that the message names no path, and
// that neither the destination nor the staging directory remains.
func assertRefused(t *testing.T, name string, err error, code int, dest, root string) {
	t.Helper()
	if !isRefusal(err, code) {
		t.Fatalf("%s: got %v, want exit code %d", name, err, code)
	}
	if strings.Contains(err.Error(), filepath.Dir(root)) || strings.Contains(err.Error(), "/") {
		t.Fatalf("%s: refusal echoes a path: %v", name, err)
	}
	for _, p := range []string{dest, filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".partial")} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: %s left behind", name, filepath.Base(p))
		}
	}
}
