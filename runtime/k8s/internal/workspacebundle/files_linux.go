package workspacebundle

import (
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

// Export returns untrusted bytes, not an atomic snapshot or provenance proof.
// The external controller must independently establish writer termination first.
// Root .git is never opened; other Git metadata, links and special nodes fail.
func Export(parent, sourceSHA string) (Bundle, error) {
	b := Bundle{Version: Version, SourceSHA: sourceSHA, Files: make([]File, 0)}
	if !ValidSourceSHA(sourceSHA) {
		return b, ErrInvalid
	}
	fd, e := unix.Open(parent, dirFlags, 0)
	if e != nil {
		return b, ErrInvalid
	}
	defer unix.Close(fd)
	root, e := unix.Openat(fd, "repository", dirFlags, 0)
	if e != nil {
		return b, ErrInvalid
	}
	defer unix.Close(root)
	nodes, total := 0, 0
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
				if nodes > MaxNodes || !validPath(name) {
					return ErrInvalid
				}
				child, e := unix.Openat(dir, entry, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
				if e != nil {
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
					if len(b.Files) >= MaxFiles || st.Nlink != 1 || st.Size < 0 || st.Size > MaxFileBytes || st.Mode&07000 != 0 {
						unix.Close(child)
						return ErrInvalid
					}
					f := os.NewFile(uintptr(child), "workspace-file")
					data, e := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
					var after unix.Stat_t
					statErr := unix.Fstat(child, &after)
					closeErr := f.Close()
					if e != nil || statErr != nil || closeErr != nil || len(data) > MaxFileBytes || int64(len(data)) != st.Size || !sameFileState(st, after) {
						return ErrInvalid
					}
					total += len(data)
					if total > MaxContentBytes {
						return ErrInvalid
					}
					mode := uint32(0644)
					if st.Mode&0100 != 0 {
						mode = 0755
					}
					b.Files = append(b.Files, File{Path: name, Mode: mode, Data: data})
				default:
					unix.Close(child)
					return ErrInvalid
				}
			}
			if e == io.EOF {
				break
			}
		}
		return nil
	}
	if walk(root, "") != nil {
		return Bundle{}, ErrInvalid
	}
	sort.Slice(b.Files, func(i, j int) bool { return b.Files[i].Path < b.Files[j].Path })
	return b, b.Validate()
}

func sameFileState(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode && a.Nlink == b.Nlink && a.Uid == b.Uid && a.Gid == b.Gid && a.Size == b.Size && a.Mtim == b.Mtim && a.Ctim == b.Ctim
}
