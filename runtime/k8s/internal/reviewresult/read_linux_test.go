package reviewresult

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func TestSummaryReaderRejectsUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"regular", "symlink", "directory", "fifo", "oversize", "run-symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			run := filepath.Join(root, "run")
			if err := os.Mkdir(run, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(run, "summary.json")
			var err error
			switch kind {
			case "regular":
				err = os.WriteFile(path, []byte(`{}`), 0600)
			case "symlink":
				err = os.Symlink("/etc/passwd", path)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "fifo":
				err = unix.Mkfifo(path, 0600)
			case "oversize":
				err = os.WriteFile(path, make([]byte, MaxBytes+1), 0600)
			case "run-symlink":
				if err = os.Remove(run); err == nil {
					err = os.Symlink(t.TempDir(), run)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := Read(root)
			if kind == "regular" {
				if err != nil || string(raw) != `{}` {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unsafe summary read")
			}
		})
	}
}
