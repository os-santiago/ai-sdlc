package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

var ErrProcess = errors.New("executor subprocess failed or exceeded a bound")

const MaxProcessOutput = 256 * 1024

// runBounded is private: production callers select fixed binaries and arguments.
// Child output is never a control-plane result and is discarded after counting.
// A PID-1 launcher must exit afterward; an external controller must observe the
// worker container terminated before it exports any candidate files.
func runBounded(ctx context.Context, binary, dir string, env, args []string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 250 * time.Millisecond
	out := &boundedDiscard{cancel: cancel}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return ErrProcess
	}
	// Kill any children still in our process group even when the direct child
	// exits successfully. Detached descendants require container termination.
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	err := cmd.Wait()
	if ctx.Err() != nil || out.exceeded {
		return ErrProcess
	}
	if err != nil {
		var exited *exec.ExitError
		if errors.As(err, &exited) {
			return exited
		}
		return ErrProcess
	}
	return nil
}

type boundedDiscard struct {
	mu       sync.Mutex
	count    int
	exceeded bool
	cancel   context.CancelFunc
}

func (w *boundedDiscard) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(p) > MaxProcessOutput-w.count {
		w.exceeded = true
		w.cancel()
		return 0, ErrProcess
	}
	w.count += len(p)
	return len(p), nil
}
