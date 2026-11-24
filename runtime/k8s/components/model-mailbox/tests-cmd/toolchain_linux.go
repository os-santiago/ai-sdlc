package main

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

// Default bounds of a copied toolchain. The byte cap matches the reviewed
// toolchain volume size; the file cap counts every entry (directories,
// files and symbolic links).
const (
	maxToolchainFiles = 50_000
	maxToolchainBytes = 1 << 30
	maxToolchainDepth = 64
	readDirBatch      = 256
)

// Exit codes of copy-toolchain. They and the messages are fixed: a refusal
// never echoes a path, a link target or file contents from the image.
const (
	exitToolchainUsage       = 2
	exitToolchainSource      = 3
	exitToolchainLink        = 4
	exitToolchainLimit       = 5
	exitToolchainDynamic     = 6
	exitToolchainDestination = 7
)

type toolchainError struct {
	code    int
	message string
}

func (e *toolchainError) Error() string { return e.message }

var (
	errToolchainUsage       = &toolchainError{exitToolchainUsage, "usage: repository-tests copy-toolchain <absolute-root> <absolute-destination>"}
	errToolchainSource      = &toolchainError{exitToolchainSource, "toolchain root unreadable or contains an unsupported entry"}
	errToolchainLink        = &toolchainError{exitToolchainLink, "toolchain contains a symbolic link that may leave its root"}
	errToolchainLimit       = &toolchainError{exitToolchainLimit, "toolchain exceeds its file, byte or depth bound"}
	errToolchainDynamic     = &toolchainError{exitToolchainDynamic, "toolchain contains a dynamically linked executable"}
	errToolchainDestination = &toolchainError{exitToolchainDestination, "toolchain destination unavailable or copy failed"}
)

type toolchainLimits struct {
	files, depth int
	bytes        int64
}

var defaultToolchainLimits = toolchainLimits{files: maxToolchainFiles, bytes: maxToolchainBytes, depth: maxToolchainDepth}

// copyToolchain copies a reviewed toolchain root from the pinned verification
// image into a volume that the generation worker mounts read-only. It runs as
// an init container of a restartPolicy Never pod, so a refusal fails the pod.
//
//	repository-tests copy-toolchain <root> <dest>
//
// Policy:
//   - The walk never follows symbolic links. The root and every one of its
//     ancestors must be real directories, so a linked ancestor cannot move
//     the walk to a path the denylist excludes.
//   - Only directories, regular files and safe internal symbolic links are
//     copied; devices, FIFOs and sockets are refused.
//   - A symbolic link is safe when its target is relative and clean, made of
//     leading ".." segments (no more than the link's depth below the root)
//     followed only by names. Such a target cannot leave the root even
//     through other safe links; absolute targets are refused because the copy
//     is relocated.
//   - Modes are normalized: directories 0555, files 0444, or 0555 when any
//     execute bit is set. Setuid, setgid and sticky bits never survive.
//   - An executable ELF file with a PT_INTERP program header (dynamically
//     linked) is refused, so that only self-contained toolchains qualify and a
//     toolchain built for another libc fails at copy time, not inside a
//     generation. This is a compatibility check, not a security boundary: the
//     generator already runs arbitrary shell in its sandbox. ELF files without
//     an execute bit, such as test data, are copied as data.
//   - Every entry counts against the file cap; regular file bytes count
//     against the byte cap; the depth is bounded.
//   - The tree is built in a staging directory beside dest and renamed into
//     place, so dest appears only complete; a failure removes the staging
//     directory. dest must not exist and its parent must be a directory.
func copyToolchain(args []string, limits toolchainLimits) error {
	if len(args) != 2 || project.ValidateToolchainRoot(args[0]) != nil || !filepath.IsAbs(args[1]) ||
		filepath.Clean(args[1]) != args[1] || args[1] == "/" || within(args[1], args[0]) || within(args[0], args[1]) {
		return errToolchainUsage
	}
	return copyTree(args[0], args[1], limits)
}

// copyTree applies the copy policy to an already validated root and dest.
func copyTree(root, dest string, limits toolchainLimits) error {
	if !realDirectoryPath(root) {
		return errToolchainSource
	}
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		return errToolchainDestination
	}
	if info, err := os.Stat(filepath.Dir(dest)); err != nil || !info.IsDir() {
		return errToolchainDestination
	}
	staging := filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".partial")
	if os.Mkdir(staging, 0o700) != nil {
		return errToolchainDestination
	}
	c := toolchainCopy{limits: limits}
	err := c.directory(root, staging, 0)
	if err == nil {
		err = c.seal()
	}
	if err == nil && os.Rename(staging, dest) != nil {
		err = errToolchainDestination
	}
	if err != nil {
		removeStaging(staging)
		return err
	}
	return nil
}

// realDirectoryPath reports whether the absolute, clean path p and each of
// its ancestors is a real directory, not a symbolic link. The source image is
// immutable while the init container runs, so checking before the walk holds
// for the whole walk; the walk itself opens every directory with O_NOFOLLOW.
func realDirectoryPath(p string) bool {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return false
	}
	current := "/"
	for _, part := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() {
			return false
		}
	}
	return true
}

// within reports whether p is base or lies under it.
func within(p, base string) bool {
	return p == base || strings.HasPrefix(p, strings.TrimSuffix(base, "/")+"/")
}

type toolchainCopy struct {
	limits toolchainLimits
	files  int
	bytes  int64
	// dirs are created writable and sealed read-only after the walk,
	// innermost first.
	dirs []string
}

func (c *toolchainCopy) count() error {
	c.files++
	if c.files > c.limits.files {
		return errToolchainLimit
	}
	return nil
}

// directory copies the entries of src (a real directory at depth below the
// root) into the existing, writable directory dst.
func (c *toolchainCopy) directory(src, dst string, depth int) error {
	if depth > c.limits.depth {
		return errToolchainLimit
	}
	c.dirs = append(c.dirs, dst)
	f, err := os.OpenFile(src, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return errToolchainSource
	}
	defer f.Close()
	for {
		// Batches bound memory for a huge directory: the file cap stops the
		// walk before the whole listing is read.
		entries, err := f.ReadDir(readDirBatch)
		for _, entry := range entries {
			if err := c.count(); err != nil {
				return err
			}
			if err := c.entry(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name()), entry.Type(), depth); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errToolchainSource
		}
	}
}

func (c *toolchainCopy) entry(src, dst string, kind os.FileMode, depth int) error {
	switch {
	case kind.IsDir():
		if os.Mkdir(dst, 0o700) != nil {
			return errToolchainDestination
		}
		return c.directory(src, dst, depth+1)
	case kind.IsRegular():
		return c.file(src, dst)
	case kind&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return errToolchainSource
		}
		if !safeLink(target, depth) {
			return errToolchainLink
		}
		if os.Symlink(target, dst) != nil {
			return errToolchainDestination
		}
		return nil
	default:
		return errToolchainSource
	}
}

// safeLink reports whether a link target, for a link in a directory depth
// levels below the root, resolves inside the root. Leading ".." segments
// climb through the link's real ancestor directories; the remaining segments
// only descend, so even a safe link along the way keeps the path inside.
func safeLink(target string, depth int) bool {
	if target == "" || len(target) > 1024 || strings.HasPrefix(target, "/") || path.Clean(target) != target ||
		strings.ContainsRune(target, 0) {
		return false
	}
	parts := strings.Split(target, "/")
	up := 0
	for up < len(parts) && parts[up] == ".." {
		up++
	}
	if up > depth {
		return false
	}
	for _, part := range parts[up:] {
		if part == ".." || part == "." || part == "" {
			return false
		}
	}
	// A target of only ".." segments names an ancestor directory inside the
	// root; that is safe but a loop, so it is refused as well.
	return up < len(parts)
}

func (c *toolchainCopy) file(src, dst string) error {
	in, err := os.OpenFile(src, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return errToolchainSource
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errToolchainSource
	}
	if info.Size() > c.limits.bytes-c.bytes {
		return errToolchainLimit
	}
	executable := info.Mode().Perm()&0o111 != 0
	if executable {
		dynamic, err := dynamicELF(in)
		if err != nil {
			return errToolchainSource
		}
		if dynamic {
			return errToolchainDynamic
		}
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return errToolchainDestination
	}
	remaining := c.limits.bytes - c.bytes
	n, copyErr := io.Copy(out, io.LimitReader(in, remaining+1))
	mode := os.FileMode(0o444)
	if executable {
		mode = 0o555
	}
	chmodErr := out.Chmod(mode)
	closeErr := out.Close()
	c.bytes += n
	switch {
	case n > remaining:
		return errToolchainLimit
	case copyErr != nil:
		return errToolchainSource
	case chmodErr != nil || closeErr != nil:
		return errToolchainDestination
	}
	return nil
}

// dynamicELF reports whether f is an ELF file with a PT_INTERP program
// header. A file that does not start with the ELF magic is not ELF; an ELF
// file whose header or program headers cannot be read is an error.
func dynamicELF(f io.ReaderAt) (bool, error) {
	var ident [16]byte
	if n, err := f.ReadAt(ident[:], 0); n < 4 || string(ident[:4]) != "\x7fELF" {
		if n < 4 && err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		return false, nil
	} else if n < len(ident) {
		return false, errToolchainSource
	}
	var order binary.ByteOrder
	switch ident[5] {
	case 1:
		order = binary.LittleEndian
	case 2:
		order = binary.BigEndian
	default:
		return false, errToolchainSource
	}
	var phoff uint64
	var phentsize, phnum, minEntry uint64
	switch ident[4] {
	case 1: // ELFCLASS32
		var h [52]byte
		if _, err := f.ReadAt(h[:], 0); err != nil {
			return false, errToolchainSource
		}
		phoff = uint64(order.Uint32(h[28:]))
		phentsize, phnum, minEntry = uint64(order.Uint16(h[42:])), uint64(order.Uint16(h[44:])), 32
	case 2: // ELFCLASS64
		var h [64]byte
		if _, err := f.ReadAt(h[:], 0); err != nil {
			return false, errToolchainSource
		}
		phoff = order.Uint64(h[32:])
		phentsize, phnum, minEntry = uint64(order.Uint16(h[54:])), uint64(order.Uint16(h[56:])), 56
	default:
		return false, errToolchainSource
	}
	if phnum == 0 {
		return false, nil
	}
	// PN_XNUM (0xffff) moves the count into a section header; real
	// executables never need it, so it is refused rather than parsed.
	if phnum == 0xffff || phentsize < minEntry || phentsize > 1024 || phoff > 1<<40 {
		return false, errToolchainSource
	}
	entry := make([]byte, 4)
	for i := uint64(0); i < phnum; i++ {
		if _, err := f.ReadAt(entry, int64(phoff+i*phentsize)); err != nil {
			return false, errToolchainSource
		}
		if order.Uint32(entry) == 3 { // PT_INTERP
			return true, nil
		}
	}
	return false, nil
}

// seal makes the copied directories read-only, innermost first.
func (c *toolchainCopy) seal() error {
	for i := len(c.dirs) - 1; i >= 0; i-- {
		if os.Chmod(c.dirs[i], 0o555) != nil {
			return errToolchainDestination
		}
	}
	return nil
}

// removeStaging removes a partial copy, first restoring write permission on
// any directory that was already sealed.
func removeStaging(staging string) {
	_ = filepath.WalkDir(staging, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	_ = os.RemoveAll(staging)
}
