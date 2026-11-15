package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	"golang.org/x/sys/unix"
)

type Start struct {
	Version          string `json:"version"`
	PodUID           string `json:"podUID"`
	AssignmentSHA256 string `json:"assignmentSHA256"`
	TreeSHA256       string `json:"treeSHA256"`
}

func (s Start) valid() bool {
	return s.Version == "ai-sdlc.executor-start/v1" && (modelmailbox.Identity{Attempt: "executor", PodUID: s.PodUID}).Validate() == nil && workspacebundle.ValidDigest(s.AssignmentSHA256) && workspacebundle.ValidDigest(s.TreeSHA256)
}
func openControl(parent string) (int, error) {
	return unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

// PublishStart is an exclusive synchronization marker, not durable authority.
// Only the external controller decides whether an assignment may be started.
func PublishStart(parent string, s Start) error {
	if !s.valid() {
		return ErrInvalid
	}
	fd, err := openControl(parent)
	if err != nil {
		return ErrInvalid
	}
	defer unix.Close(fd)
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return ErrInvalid
	}
	tmp := ".start-" + hex.EncodeToString(nonce[:])
	file, err := unix.Openat(fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return ErrInvalid
	}
	defer unix.Unlinkat(fd, tmp, 0)
	f := os.NewFile(uintptr(file), "start")
	raw, _ := json.Marshal(s)
	_, writeErr := f.Write(raw)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return ErrInvalid
	}
	if unix.Linkat(fd, tmp, fd, "start.json", 0) != nil {
		return ErrInvalid
	}
	if unix.Unlinkat(fd, tmp, 0) != nil {
		return ErrInvalid
	}
	return nil
}

// WaitStart never deletes or rewrites the marker. Once SCC starts every file in
// its writable volumes is untrusted; the controller must use external state.
func WaitStart(ctx context.Context, parent string, expected Start) error {
	if !expected.valid() {
		return ErrInvalid
	}
	fd, err := openControl(parent)
	if err != nil {
		return ErrInvalid
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
		file, e := unix.Openat(fd, "start.json", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if errors.Is(e, unix.ENOENT) {
			timer.Reset(25 * time.Millisecond)
			continue
		}
		if e != nil {
			return ErrInvalid
		}
		f := os.NewFile(uintptr(file), "start")
		var before, after unix.Stat_t
		if unix.Fstat(file, &before) != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Size > 1024 {
			f.Close()
			return ErrInvalid
		}
		if before.Nlink != 1 {
			f.Close()
			timer.Reset(25 * time.Millisecond)
			continue
		}
		raw, e := io.ReadAll(io.LimitReader(f, 1025))
		statErr := unix.Fstat(file, &after)
		f.Close()
		if e != nil || statErr != nil || len(raw) > 1024 || int64(len(raw)) != after.Size || before.Size != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim || after.Nlink != 1 {
			return ErrInvalid
		}
		var observed Start
		keys := []string{"version", "podUID", "assignmentSHA256", "treeSHA256"}
		if strictBound(raw, workspacebundle.Digest(raw), 1024, keys, &observed) != nil || observed != expected {
			return ErrInvalid
		}
		return nil
	}
}
