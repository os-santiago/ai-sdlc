package reviewresult

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Read opens only run/summary.json below the trusted state mount. Neither the
// directory nor the file can be a symlink, FIFO, device or oversized artifact.
// The caller must observe the original writer terminated before and afterward.
func Read(stateDirectory string) ([]byte, error) {
	root, err := unix.Open(stateDirectory, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrInvalid
	}
	defer unix.Close(root)
	run, err := unix.Openat(root, "run", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrInvalid
	}
	defer unix.Close(run)
	fd, err := unix.Openat(run, "summary.json", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrInvalid
	}
	f := os.NewFile(uintptr(fd), "summary.json")
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() < 1 || st.Size() > MaxBytes {
		return nil, ErrInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil || len(raw) < 1 || len(raw) > MaxBytes {
		return nil, ErrInvalid
	}
	return raw, nil
}
