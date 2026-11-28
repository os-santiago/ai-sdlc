package executor

import (
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	"golang.org/x/sys/unix"
)

// VerifySource runs in the worker's own filesystem view before any untrusted
// repository code starts. The production caller supplies the fixed mount path.
// Export intentionally omits root .git, so its absence needs a separate check.
func VerifySource(parent string, a Assignment) error {
	fd, e := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return ErrInvalid
	}
	defer unix.Close(fd)
	root, e := unix.Openat(fd, "repository", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return ErrInvalid
	}
	defer unix.Close(root)
	var st unix.Stat_t
	if e = unix.Fstatat(root, ".git", &st, unix.AT_SYMLINK_NOFOLLOW); e != unix.ENOENT {
		return ErrInvalid
	}
	// A prior local baseline must not be reused, even if its worktree marker
	// disappeared. This path is outside the regular-file candidate export.
	if e = unix.Fstatat(fd, "git", &st, unix.AT_SYMLINK_NOFOLLOW); e != unix.ENOENT {
		return ErrInvalid
	}
	// Hashes one file at a time instead of holding the tree in memory.
	summary, e := workspacebundle.ExportSummary(parent, a.SourceSHA)
	if e != nil || summary.TreeDigest != a.TreeSHA256 {
		return ErrInvalid
	}
	return nil
}
