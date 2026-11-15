package modelmailbox

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const requestFile = "request.json"
const responseFile = "response.json"

// ErrPending includes the brief link/unlink publication window. Linked file
// bytes are never read, even if an adversary leaves the extra link indefinitely.
var ErrPending = errors.New("mailbox publication is pending")

// Store keeps an open directory descriptor. All subsequent filesystem access
// uses flat, internal names relative to it, never a model-controlled pathname.
type Store struct{ dir *os.File }

func OpenStore(path string) (*Store, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("cannot open mailbox directory")
	}
	return &Store{dir: os.NewFile(uintptr(fd), "mailbox")}, nil
}
func (s *Store) Close() error { return s.dir.Close() }

func (s *Store) read(name string) ([]byte, error) {
	if name != requestFile && name != responseFile {
		return nil, errors.New("invalid mailbox filename")
	}
	fd, err := syscall.Openat(int(s.dir.Fd()), name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, errors.New("cannot open mailbox envelope")
	}
	f := os.NewFile(uintptr(fd), "envelope")
	defer f.Close()
	var before, after syscall.Stat_t
	if syscall.Fstat(fd, &before) != nil || before.Mode&syscall.S_IFMT != syscall.S_IFREG || before.Size > MaxEnvelopeBytes {
		return nil, errors.New("unsafe mailbox envelope")
	}
	if before.Nlink != 1 {
		return nil, ErrPending
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxEnvelopeBytes+1))
	if err != nil || len(raw) > MaxEnvelopeBytes || syscall.Fstat(fd, &after) != nil || after.Nlink != 1 || before.Size != after.Size || int64(len(raw)) != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim {
		return nil, errors.New("mailbox envelope changed or exceeded limit")
	}
	return raw, nil
}

func (s *Store) publish(name string, e Envelope) error {
	if name != requestFile && name != responseFile {
		return errors.New("invalid mailbox filename")
	}
	if _, err := s.read(name); !errors.Is(err, os.ErrNotExist) {
		return errors.New("mailbox destination already exists or is unsafe")
	}
	raw, err := json.Marshal(e)
	if err != nil || len(raw) > MaxEnvelopeBytes {
		return errors.New("invalid mailbox envelope size")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errors.New("cannot allocate mailbox staging file")
	}
	tmp := ".publish-" + hex.EncodeToString(nonce[:])
	fd, err := syscall.Openat(int(s.dir.Fd()), tmp, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return errors.New("cannot stage mailbox envelope")
	}
	f := os.NewFile(uintptr(fd), "staging")
	defer syscall.Unlinkat(int(s.dir.Fd()), tmp)
	_, writeErr := f.Write(raw)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errors.New("cannot write mailbox envelope")
	}
	// linkat atomically creates an entry without replacing an existing winner.
	// Readers wait until staging is unlinked and the final file has one link.
	// This also works where gVisor's backing filesystem rejects renameat2 flags.
	if err := unix.Linkat(int(s.dir.Fd()), tmp, int(s.dir.Fd()), name, 0); err != nil {
		return fmt.Errorf("cannot publish mailbox envelope: %w", err)
	}
	if err := syscall.Unlinkat(int(s.dir.Fd()), tmp); err != nil {
		return errors.New("cannot finalize mailbox publication")
	}
	return nil
}

func (s *Store) remove(name string) error {
	if name != requestFile && name != responseFile {
		return errors.New("invalid mailbox filename")
	}
	err := syscall.Unlinkat(int(s.dir.Fd()), name)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	return err
}

func (s *Store) ReadRequest(id Identity) (Envelope, error) {
	raw, err := s.read(requestFile)
	if err != nil {
		return Envelope{}, err
	}
	e, err := Decode(raw)
	if err != nil {
		return e, err
	}
	return e, e.Validate(id, "request")
}

func (s *Store) WriteResponse(id Identity, response Envelope) error {
	if err := response.Validate(id, "response"); err != nil {
		return err
	}
	request, err := s.ReadRequest(id)
	if err != nil {
		return errors.New("response has no valid pending request")
	}
	if response.Sequence != request.Sequence || response.RequestSHA256 != request.RequestSHA256 || response.ExpiresAt != request.ExpiresAt {
		return errors.New("response does not match pending request")
	}
	return s.publish(responseFile, response)
}
