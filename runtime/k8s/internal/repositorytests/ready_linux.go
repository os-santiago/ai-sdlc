package repositorytests

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// ReadyVersion names the marker the transfer sidecar publishes once the
// controller has imported and checked the source tree of one run.
const ReadyVersion = "ai-sdlc.repository-tests-ready/v1"

const readyName = "ready.json"

// MaxReadyBytes bounds the marker file.
const MaxReadyBytes = 1024

// ReadySeconds bounds the runner's wait for the marker, which covers the
// controller's import and ready steps. A ready step that times out leaves the
// pod unusable (a half-published marker is never accepted, and a second ready
// is refused); the controller must fail the run, not retry it.
const ReadySeconds = 120

var ErrReady = errors.New("invalid repository tests ready marker")

// Ready binds one run to its pod and its imported tree. It is a
// synchronization marker inside the pod, never authority: the controller
// alone decides what was imported and whether the run's result counts.
type Ready struct {
	Version    string `json:"version"`
	PodUID     string `json:"podUID"`
	SourceSHA  string `json:"sourceSHA"`
	TreeSHA256 string `json:"treeSHA256"`
}

func (r Ready) valid() bool {
	return r.Version == ReadyVersion && (modelmailbox.Identity{Attempt: "repository-tests", PodUID: r.PodUID}).Validate() == nil &&
		workspacebundle.ValidSourceSHA(r.SourceSHA) && workspacebundle.ValidDigest(r.TreeSHA256)
}

func openParent(parent string) (int, error) {
	return unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

// PublishReady writes the marker exclusively: it is written and synced under
// a random name and then hard-linked into place, so a reader never sees a
// partial marker, and an existing marker is never replaced.
func PublishReady(parent string, r Ready) error {
	if !r.valid() {
		return ErrReady
	}
	fd, err := openParent(parent)
	if err != nil {
		return ErrReady
	}
	defer unix.Close(fd)
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return ErrReady
	}
	tmp := ".ready-" + hex.EncodeToString(nonce[:])
	file, err := unix.Openat(fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return ErrReady
	}
	defer unix.Unlinkat(fd, tmp, 0)
	f := os.NewFile(uintptr(file), "ready")
	raw, _ := json.Marshal(r)
	_, writeErr := f.Write(raw)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return ErrReady
	}
	if unix.Linkat(fd, tmp, fd, readyName, 0) != nil || unix.Unlinkat(fd, tmp, 0) != nil {
		return ErrReady
	}
	return nil
}

// WaitReady polls until the marker exists and equals expected exactly. It
// skips a marker that is still linked under its temporary name, and refuses
// anything that is not a small, stable regular file.
func WaitReady(ctx context.Context, parent string, expected Ready) error {
	if !expected.valid() {
		return ErrReady
	}
	fd, err := openParent(parent)
	if err != nil {
		return ErrReady
	}
	defer unix.Close(fd)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		file, e := unix.Openat(fd, readyName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if errors.Is(e, unix.ENOENT) {
			timer.Reset(25 * time.Millisecond)
			continue
		}
		if e != nil {
			return ErrReady
		}
		f := os.NewFile(uintptr(file), "ready")
		var before, after unix.Stat_t
		if unix.Fstat(file, &before) != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Size > MaxReadyBytes {
			f.Close()
			return ErrReady
		}
		if before.Nlink != 1 {
			f.Close()
			timer.Reset(25 * time.Millisecond)
			continue
		}
		raw, e := io.ReadAll(io.LimitReader(f, MaxReadyBytes+1))
		statErr := unix.Fstat(file, &after)
		f.Close()
		if e != nil || statErr != nil || len(raw) > MaxReadyBytes || int64(len(raw)) != after.Size || before.Size != after.Size ||
			before.Mtim != after.Mtim || before.Ctim != after.Ctim || after.Nlink != 1 {
			return ErrReady
		}
		var observed Ready
		if authority.DecodeStrict(raw, &observed) != nil || observed != expected {
			return ErrReady
		}
		return nil
	}
}

// VerifyTree checks, before any repository code runs, that parent/repository
// is exactly the imported tree: no root .git, no leftover git directory and
// the tree digest the controller expects.
func VerifyTree(parent, sourceSHA, treeSHA256 string) error {
	fd, e := openParent(parent)
	if e != nil {
		return ErrReady
	}
	defer unix.Close(fd)
	root, e := unix.Openat(fd, "repository", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return ErrReady
	}
	defer unix.Close(root)
	var st unix.Stat_t
	if unix.Fstatat(root, ".git", &st, unix.AT_SYMLINK_NOFOLLOW) != unix.ENOENT || unix.Fstatat(fd, "git", &st, unix.AT_SYMLINK_NOFOLLOW) != unix.ENOENT {
		return ErrReady
	}
	// Hashes one file at a time: the transfer sidecar's memory does not grow
	// with the repository.
	summary, e := workspacebundle.ExportSummary(parent, sourceSHA)
	if e != nil || summary.TreeDigest != treeSHA256 {
		return ErrReady
	}
	return nil
}
