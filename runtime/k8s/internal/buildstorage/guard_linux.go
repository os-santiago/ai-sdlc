// Package buildstorage checks installed local-kind storage custody. It never
// allocates, repairs, reclaims or changes a workspace and never executes source.
package buildstorage

import (
	"errors"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"golang.org/x/sys/unix"
)

const Capacity = 4 << 30
const Inodes = 32768
const MarkerName = "custody.json"

var digest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var uuid = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var ErrCustody = errors.New("installed build workspace custody denied")

type Marker struct {
	Version               string   `json:"version"`
	StorageIdentitySHA256 string   `json:"storageIdentitySHA256"`
	NodeBootID            string   `json:"nodeBootID"`
	FSID                  [2]int32 `json:"fsid"`
	CapacityBytes         uint64   `json:"capacityBytes"`
	InodeLimit            uint64   `json:"inodeLimit"`
}
type Proof struct {
	Version               string `json:"version"`
	StorageIdentitySHA256 string `json:"storageIdentitySHA256"`
	NodeBootID            string `json:"nodeBootID"`
	Phase                 string `json:"phase"`
	CapacityBytes         uint64 `json:"capacityBytes"`
	InodeLimit            uint64 `json:"inodeLimit"`
}

func nodeBootID() (string, error) {
	f, e := os.Open("/proc/sys/kernel/random/boot_id")
	if e != nil {
		return "", ErrCustody
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, 65))
	id := strings.TrimSpace(string(raw))
	if e != nil || len(raw) > 64 || !uuid.MatchString(id) {
		return "", ErrCustody
	}
	return id, nil
}
func filesystem(fd int) (unix.Statfs_t, error) {
	var fs unix.Statfs_t
	if unix.Fstatfs(fd, &fs) != nil || fs.Type != unix.TMPFS_MAGIC || fs.Bsize <= 0 || fs.Blocks > Capacity || uint64(fs.Bsize) > Capacity || fs.Blocks != uint64(Capacity)/uint64(fs.Bsize) || fs.Blocks*uint64(fs.Bsize) != Capacity || fs.Files != Inodes {
		return fs, ErrCustody
	}
	return fs, nil
}

// Inspect requires a fixed mount root, not a source-controlled path. The
// installer creates root-owned immutable custody metadata before any writer.
// The isolated writer owns only the workspace child. API/PV/Pod admission and
// writer termination are separate controller obligations, not proven here.
func Inspect(path, identity, boot, phase string) (Proof, error) {
	var out Proof
	if path != "/slot" || !digest.MatchString(identity) || !uuid.MatchString(boot) || (phase != "build" && phase != "publish") {
		return out, ErrCustody
	}
	actual, e := nodeBootID()
	if e != nil || actual != boot {
		return out, ErrCustody
	}
	fd, e := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return out, ErrCustody
	}
	defer unix.Close(fd)
	var root unix.Stat_t
	if unix.Fstat(fd, &root) != nil || root.Uid != 0 || root.Gid != 0 || root.Mode&0o7777 != 0o555 {
		return out, ErrCustody
	}
	fs, e := filesystem(fd)
	if e != nil || (phase == "publish") != (fs.Flags&unix.ST_RDONLY != 0) {
		return out, ErrCustody
	}
	markerFD, e := unix.Openat(fd, MarkerName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if e != nil {
		return out, ErrCustody
	}
	file := os.NewFile(uintptr(markerFD), MarkerName)
	defer file.Close()
	var before, after unix.Stat_t
	if unix.Fstat(markerFD, &before) != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Mode&0o7777 != 0o444 || before.Uid != 0 || before.Gid != 0 || before.Nlink != 1 || before.Dev != root.Dev || before.Size < 1 || before.Size > 2048 {
		return out, ErrCustody
	}
	raw, e := io.ReadAll(io.LimitReader(file, 2049))
	var marker Marker
	if e != nil || int64(len(raw)) != before.Size || authority.DecodeStrict(raw, &marker) != nil || unix.Fstat(markerFD, &after) != nil || !stable(before, after) {
		return out, ErrCustody
	}
	if marker.Version != "ai-sdlc.local-build-custody/v1" || marker.StorageIdentitySHA256 != identity || marker.NodeBootID != boot || marker.FSID != fs.Fsid.Val || marker.CapacityBytes != Capacity || marker.InodeLimit != Inodes {
		return out, ErrCustody
	}
	workFD, e := unix.Openat(fd, "workspace", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return out, ErrCustody
	}
	work := os.NewFile(uintptr(workFD), "workspace")
	defer work.Close()
	var child unix.Stat_t
	if unix.Fstat(workFD, &child) != nil || child.Uid != 1000 || child.Gid != 1000 || child.Mode&0o7777 != 0o700 || child.Dev != root.Dev {
		return out, ErrCustody
	}
	childFS, e := filesystem(workFD)
	if e != nil || childFS.Fsid != fs.Fsid {
		return out, ErrCustody
	}
	if phase == "build" {
		entries, e := work.ReadDir(1)
		if (e != nil && e != io.EOF) || len(entries) != 0 {
			return out, ErrCustody
		}
	}
	var final unix.Stat_t
	var finalFS unix.Statfs_t
	if unix.Fstat(fd, &final) != nil || !stable(root, final) || unix.Fstatfs(fd, &finalFS) != nil || finalFS.Fsid != fs.Fsid || (phase == "publish") != (finalFS.Flags&unix.ST_RDONLY != 0) {
		return out, ErrCustody
	}
	return Proof{Version: "ai-sdlc.local-build-custody-proof/v1", StorageIdentitySHA256: identity, NodeBootID: boot, Phase: phase, CapacityBytes: Capacity, InodeLimit: Inodes}, nil
}

func stable(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode && a.Uid == b.Uid && a.Gid == b.Gid && a.Nlink == b.Nlink && a.Size == b.Size && a.Mtim == b.Mtim && a.Ctim == b.Ctim
}
