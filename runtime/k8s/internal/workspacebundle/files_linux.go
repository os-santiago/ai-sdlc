package workspacebundle

import (
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK

// Import validates the entire tree before creating repository. Any existing or
// partially imported directory is refused; recovery discards the owned volume.
// The caller supplies a trusted fixed parent path, never a bundle-controlled one.
func Import(parent string, b Bundle) error {
	if b.Validate() != nil {
		return ErrInvalid
	}
	fd, e := unix.Open(parent, dirFlags, 0)
	if e != nil {
		return ErrInvalid
	}
	defer unix.Close(fd)
	if unix.Mkdirat(fd, "repository", 0750) != nil {
		return ErrInvalid
	}
	root, e := unix.Openat(fd, "repository", dirFlags, 0)
	if e != nil {
		return ErrInvalid
	}
	defer unix.Close(root)
	for _, f := range b.Files {
		if writeFile(root, f) != nil {
			return ErrInvalid
		}
	}
	if unix.Fsync(root) != nil {
		return ErrInvalid
	}
	return nil
}
func writeFile(root int, f File) error {
	parent, e := unix.Dup(root)
	if e != nil {
		return ErrInvalid
	}
	defer func() { unix.Close(parent) }()
	parts := strings.Split(f.Path, "/")
	for _, part := range parts[:len(parts)-1] {
		if e = unix.Mkdirat(parent, part, 0755); e != nil && e != unix.EEXIST {
			return ErrInvalid
		}
		next, e := unix.Openat(parent, part, dirFlags, 0)
		if e != nil {
			return ErrInvalid
		}
		unix.Close(parent)
		parent = next
	}
	fd, e := unix.Openat(parent, parts[len(parts)-1], unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return ErrInvalid
	}
	file := os.NewFile(uintptr(fd), "workspace-file")
	defer file.Close()
	if _, e = file.Write(f.Data); e != nil {
		return ErrInvalid
	}
	if file.Chmod(os.FileMode(f.Mode)) != nil || file.Sync() != nil || file.Close() != nil {
		return ErrInvalid
	}
	return nil
}

// ImportFrom streams one bundle from input into parent/repository and returns
// its identity. It holds the paths and one decoded file at a time, never the
// whole wire document or tree. Files are written to a fresh staging directory
// next to repository; repository appears, by one rename, only after the whole
// document was parsed, the tree checks passed, the wire SHA-256 equals
// expectedDigest and the document names sourceSHA. A refusal removes the
// staging directory, so repository never exists for refused input and a
// later valid import can still run.
func ImportFrom(parent string, input io.Reader, expectedDigest, sourceSHA string) (Summary, error) {
	if input == nil || !ValidDigest(expectedDigest) || !ValidSourceSHA(sourceSHA) {
		return Summary{}, ErrInvalid
	}
	fd, e := unix.Open(parent, dirFlags, 0)
	if e != nil {
		return Summary{}, ErrInvalid
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if unix.Fstatat(fd, "repository", &st, unix.AT_SYMLINK_NOFOLLOW) != unix.ENOENT {
		return Summary{}, ErrInvalid
	}
	// A staging directory left by an earlier refused or interrupted import is
	// removed first, so a refusal never blocks a later valid import.
	if removeTree(fd, stagingName) != nil || unix.Mkdirat(fd, stagingName, 0750) != nil {
		return Summary{}, ErrInvalid
	}
	staging, e := unix.Openat(fd, stagingName, dirFlags, 0)
	if e != nil {
		return Summary{}, ErrInvalid
	}
	wire := newWireReader(input)
	summary, _, e := scan(wire, true, func(f File) error { return writeFile(staging, f) })
	if e != nil || wire.err != nil || wire.digest() != expectedDigest || summary.SourceSHA != sourceSHA || unix.Fsync(staging) != nil {
		unix.Close(staging)
		removeTree(fd, stagingName)
		return Summary{}, ErrInvalid
	}
	unix.Close(staging)
	if unix.Renameat(fd, stagingName, fd, "repository") != nil {
		removeTree(fd, stagingName)
		return Summary{}, ErrInvalid
	}
	// An error after the rename must not leave repository published: remove
	// it, so the failure is a clean refusal and a retry can import again.
	if unix.Fsync(fd) != nil {
		removeTree(fd, "repository")
		return Summary{}, ErrInvalid
	}
	return summary, nil
}

// removeTree deletes name below the directory dirfd and everything in it,
// opening every directory without following links and unlinking entries by
// descriptor, so a link inside is removed, never followed. A missing name is
// not an error. The import calls it only for its own staging directory.
func removeTree(dirfd int, name string) error {
	var st unix.Stat_t
	if e := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); e == unix.ENOENT {
		return nil
	} else if e != nil {
		return ErrInvalid
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unix.Unlinkat(dirfd, name, 0)
	}
	child, e := unix.Openat(dirfd, name, dirFlags, 0)
	if e != nil {
		return ErrInvalid
	}
	copied, e := unix.Dup(child)
	if e != nil {
		unix.Close(child)
		return ErrInvalid
	}
	directory := os.NewFile(uintptr(copied), "workspace-staging")
	for {
		entries, e := directory.Readdirnames(128)
		for _, entry := range entries {
			if removeTree(child, entry) != nil {
				directory.Close()
				unix.Close(child)
				return ErrInvalid
			}
		}
		if e == io.EOF || (e == nil && len(entries) == 0) {
			break
		}
		if e != nil {
			directory.Close()
			unix.Close(child)
			return ErrInvalid
		}
	}
	directory.Close()
	unix.Close(child)
	if unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR) != nil {
		return ErrInvalid
	}
	return nil
}

// stagingName is the fixed directory ImportFrom fills before renaming it to
// repository. Its leading dot keeps it apart from the repository name.
const stagingName = ".repository-import"

// Export returns untrusted bytes, not an atomic snapshot or provenance proof.
// The external controller must independently establish writer termination first.
// Root .git is never opened; other Git metadata, links and special nodes fail.
func Export(parent, sourceSHA string) (Bundle, error) {
	b := Bundle{Version: Version, SourceSHA: sourceSHA, Files: make([]File, 0)}
	e := walkTree(parent, sourceSHA, func(f File) error {
		f.Data = append(make([]byte, 0, len(f.Data)), f.Data...)
		b.Files = append(b.Files, f)
		return nil
	})
	if e != nil {
		return Bundle{}, e
	}
	return b, b.Validate()
}

// ExportTo writes Encode(Export(parent, sourceSHA)) to w while reading one file
// at a time. Every refusal reason is found before the first byte is written; a
// file that changes between the listing and its read fails with a partial
// document on w, which the caller must discard (the helper exits nonzero).
func ExportTo(parent, sourceSHA string, w io.Writer) error {
	var enc *encoder
	return walkTree(parent, sourceSHA, func(f File) error {
		if enc == nil {
			var e error
			if enc, e = newEncoder(w, sourceSHA); e != nil {
				return ErrInvalid
			}
		}
		return enc.file(f)
	}, func() error {
		if enc == nil {
			var e error
			if enc, e = newEncoder(w, sourceSHA); e != nil {
				return ErrInvalid
			}
		}
		return enc.end()
	})
}

// ExportSummary returns the identity Export(parent, sourceSHA) would have,
// tree digest included, holding one file's bytes at a time.
func ExportSummary(parent, sourceSHA string) (Summary, error) {
	s := Summary{SourceSHA: sourceSHA}
	entries := []entry{}
	e := walkTree(parent, sourceSHA, func(f File) error {
		entries = append(entries, entry{path: f.Path, mode: f.Mode, size: len(f.Data), sum: sha256.Sum256(f.Data)})
		s.ContentBytes += len(f.Data)
		return nil
	})
	if e != nil {
		return Summary{}, e
	}
	s.Files, s.TreeDigest = len(entries), treeDigest(sourceSHA, entries)
	return s, nil
}

// listed is one regular file found by the listing pass, with the state its
// read must still observe.
type listed struct {
	path string
	mode uint32
	st   unix.Stat_t
}

// walkTree lists parent/repository without reading file contents, checks
// every bound and the tree, then reads the files in path order and passes
// each to visit, followed by done (when given). A read reopens the file
// component by component without following links and requires the same
// device, inode, mode, link count, owner, size and change times as the
// listing, before and after reading. visit must copy Data to keep it.
func walkTree(parent, sourceSHA string, visit func(File) error, done ...func() error) error {
	if !ValidSourceSHA(sourceSHA) {
		return ErrInvalid
	}
	fd, e := unix.Open(parent, dirFlags, 0)
	if e != nil {
		return ErrInvalid
	}
	defer unix.Close(fd)
	root, e := unix.Openat(fd, "repository", dirFlags, 0)
	if e != nil {
		return ErrInvalid
	}
	defer unix.Close(root)
	files, e := listTree(root)
	if e != nil {
		var refused exportRefused
		if errors.As(e, &refused) {
			return refused
		}
		return ErrInvalid
	}
	names := make(map[string]bool, len(files))
	for _, f := range files {
		names[f.path] = true
	}
	if checkTree(names) != nil {
		return ErrInvalid
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	// The wire size is known from the listing too (JSON escaping can grow a
	// path sixfold), so an encoding over MaxWireBytes is refused here, with
	// its reason, before anything is read or written.
	sized := make([]entry, len(files))
	for i, f := range files {
		sized[i] = entry{path: f.path, mode: f.mode, size: int(f.st.Size)}
	}
	if canonicalSize(sourceSHA, sized) > MaxWireBytes {
		return exportRefused{ExportTotalSize}
	}
	buffer := make([]byte, 0, 64*1024)
	for _, f := range files {
		data, e := readListed(root, f, buffer)
		if e != nil {
			return ErrInvalid
		}
		buffer = data[:0]
		if visit(File{Path: f.path, Mode: f.mode, Data: data}) != nil {
			return ErrInvalid
		}
	}
	for _, finish := range done {
		if finish() != nil {
			return ErrInvalid
		}
	}
	return nil
}

func listTree(root int) ([]listed, error) {
	var files []listed
	nodes, total := 0, int64(0)
	var walk func(int, string) error
	walk = func(dir int, prefix string) error {
		copied, e := unix.Dup(dir)
		if e != nil {
			return ErrInvalid
		}
		directory := os.NewFile(uintptr(copied), "workspace-directory")
		defer directory.Close()
		// Read names only, then inspect every observed entry via this directory
		// FD. Do not silently skip entries whose metadata becomes unavailable.
		// Batches also bound hostile directories with many empty children.
		for {
			entries, e := directory.Readdirnames(128)
			if e != nil && e != io.EOF {
				return ErrInvalid
			}
			for _, entry := range entries {
				if prefix == "" && entry == ".git" {
					continue
				}
				nodes++
				name := entry
				if prefix != "" {
					name = prefix + "/" + name
				}
				if nodes > MaxNodes {
					return exportRefused{ExportTooMany}
				}
				if !ValidPath(name) {
					return exportRefused{ExportPath}
				}
				child, e := unix.Openat(dir, entry, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
				if e != nil {
					// ELOOP: a symbolic link (O_NOFOLLOW); ENXIO: a UNIX socket.
					if errors.Is(e, unix.ELOOP) || errors.Is(e, unix.ENXIO) {
						return exportRefused{ExportNode}
					}
					return ErrInvalid
				}
				var st unix.Stat_t
				if unix.Fstat(child, &st) != nil {
					unix.Close(child)
					return ErrInvalid
				}
				switch st.Mode & unix.S_IFMT {
				case unix.S_IFDIR:
					e = walk(child, name)
					unix.Close(child)
					if e != nil {
						return e
					}
				case unix.S_IFREG:
					unix.Close(child)
					if len(files) >= MaxFiles {
						return exportRefused{ExportTooMany}
					}
					if st.Nlink != 1 || st.Mode&07000 != 0 {
						return exportRefused{ExportNode}
					}
					if st.Size < 0 || st.Size > MaxFileBytes {
						return exportRefused{ExportFileSize}
					}
					total += st.Size
					if total > MaxContentBytes {
						return exportRefused{ExportTotalSize}
					}
					mode := uint32(0644)
					if st.Mode&0100 != 0 {
						mode = 0755
					}
					files = append(files, listed{path: name, mode: mode, st: st})
				default:
					unix.Close(child)
					return exportRefused{ExportNode}
				}
			}
			if e == io.EOF {
				break
			}
		}
		return nil
	}
	if e := walk(root, ""); e != nil {
		return nil, e
	}
	return files, nil
}

// readListed reopens one listed file below root without following links and
// reads exactly its listed size into buffer's storage when it fits.
func readListed(root int, f listed, buffer []byte) ([]byte, error) {
	parent, e := unix.Dup(root)
	if e != nil {
		return nil, ErrInvalid
	}
	defer func() { unix.Close(parent) }()
	parts := strings.Split(f.path, "/")
	for _, part := range parts[:len(parts)-1] {
		next, e := unix.Openat(parent, part, dirFlags, 0)
		if e != nil {
			return nil, ErrInvalid
		}
		unix.Close(parent)
		parent = next
	}
	child, e := unix.Openat(parent, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if e != nil {
		return nil, ErrInvalid
	}
	file := os.NewFile(uintptr(child), "workspace-file")
	defer file.Close()
	var before, after unix.Stat_t
	if unix.Fstat(child, &before) != nil || !sameFileState(f.st, before) {
		return nil, ErrInvalid
	}
	size := int(f.st.Size)
	if cap(buffer) < size+1 {
		buffer = make([]byte, 0, size+1)
	}
	data := buffer[:size+1]
	n, e := io.ReadFull(file, data)
	if e != io.ErrUnexpectedEOF && !(e == io.EOF && size == 0) || n != size {
		return nil, ErrInvalid
	}
	if unix.Fstat(child, &after) != nil || !sameFileState(f.st, after) {
		return nil, ErrInvalid
	}
	return data[:size], nil
}

func sameFileState(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode && a.Nlink == b.Nlink && a.Uid == b.Uid && a.Gid == b.Gid && a.Size == b.Size && a.Mtim == b.Mtim && a.Ctim == b.Ctim
}

// ExportReason names why Export refused a workspace. Every reason is still
// ErrInvalid; the reason only lets the export helper exit with a bounded code
// (evaluation battery 31 lost a finished candidate at export-helper-exit-1,
// and nothing said which rule refused it).
type ExportReason int

const (
	// ExportPath: a path outside the bundle's path rules (ValidPath).
	ExportPath ExportReason = iota + 3
	// ExportNode: a link, a hard link, a special node or a file with setuid,
	// setgid or sticky bits.
	ExportNode
	// ExportFileSize: one file over MaxFileBytes, for example a compiled
	// test binary left in the workspace.
	ExportFileSize
	// ExportTotalSize: the content over MaxContentBytes, or its encoding over
	// MaxWireBytes.
	ExportTotalSize
	// ExportTooMany: over MaxNodes entries or MaxFiles files.
	ExportTooMany
)

type exportRefused struct{ reason ExportReason }

func (e exportRefused) Error() string { return ErrInvalid.Error() }
func (e exportRefused) Unwrap() error { return ErrInvalid }

// RefusalReason returns why Export refused, or 0 for any other error.
func RefusalReason(err error) ExportReason {
	var refused exportRefused
	if errors.As(err, &refused) {
		return refused.reason
	}
	return 0
}
