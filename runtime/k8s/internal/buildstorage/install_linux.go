package buildstorage

import (
	"encoding/json"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const InstallationRoot = "/var/local/ai-sdlc/build-slots/"

// PrepareInstalled initializes exactly one empty, already mounted, bounded tmpfs.
// It never mounts, erases, repairs or authorizes reuse of a consumed slot. Reapply
// validates complete custody and preserves workspace contents. Partial writes
// remain denied for explicit administrative investigation.
func PrepareInstalled(identity, boot string) (Proof, error) {
	var denied Proof
	if os.Geteuid() != 0 || !digest.MatchString(identity) || !uuid.MatchString(boot) {
		return denied, ErrCustody
	}
	actual, e := nodeBootID()
	if e != nil || actual != boot {
		return denied, ErrCustody
	}
	parent, e := unix.Openat2(unix.AT_FDCWD, InstallationRoot, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS})
	if e != nil {
		return denied, ErrCustody
	}
	defer unix.Close(parent)
	fd, e := unix.Openat2(parent, identity, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS})
	if e != nil {
		return denied, ErrCustody
	}
	f := os.NewFile(uintptr(fd), identity)
	defer f.Close()
	if unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) != nil {
		return denied, ErrCustody
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	var root, parentStat unix.Stat_t
	fs, e := filesystem(fd)
	if e != nil || fs.Flags&unix.ST_RDONLY != 0 || unix.Fstat(fd, &root) != nil || unix.Fstat(parent, &parentStat) != nil || root.Dev == parentStat.Dev || root.Uid != 0 || root.Gid != 0 {
		return denied, ErrCustody
	}
	if root.Mode&0o7777 == 0o555 {
		return inspectFD(fd, identity, boot, "installed")
	}
	if root.Mode&0o7777 != 0o755 {
		return denied, ErrCustody
	}
	entries, e := f.ReadDir(1)
	if (e != nil && e != io.EOF) || len(entries) != 0 {
		return denied, ErrCustody
	}
	raw, _ := json.Marshal(Marker{Version: "ai-sdlc.local-build-custody/v1", StorageIdentitySHA256: identity, NodeBootID: boot, FSID: fs.Fsid.Val, CapacityBytes: Capacity, InodeLimit: Inodes})
	mfd, e := unix.Openat(fd, MarkerName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o444)
	if e != nil {
		return denied, ErrCustody
	}
	marker := os.NewFile(uintptr(mfd), MarkerName)
	n, writeErr := marker.Write(raw)
	syncErr := marker.Sync()
	closeErr := marker.Close()
	if writeErr != nil || n != len(raw) || syncErr != nil || closeErr != nil || unix.Mkdirat(fd, "workspace", 0o700) != nil {
		return denied, ErrCustody
	}
	wfd, e := unix.Openat(fd, "workspace", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return denied, ErrCustody
	}
	chownErr := unix.Fchown(wfd, 1000, 1000)
	modeErr := unix.Fchmod(wfd, 0o700)
	unix.Close(wfd)
	if chownErr != nil || modeErr != nil || unix.Fchmod(fd, 0o555) != nil || unix.Fsync(fd) != nil {
		return denied, ErrCustody
	}
	return inspectFD(fd, identity, boot, "installed")
}
