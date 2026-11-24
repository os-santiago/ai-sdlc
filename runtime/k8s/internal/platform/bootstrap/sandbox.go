package bootstrap

import (
	"context"
	"errors"
	"log"
	"sync"
)

// SandboxGate guards a component that creates sandboxed pods. Check runs the
// read-only sandbox RuntimeClass preflight; while it fails the component is
// not ready and claims no work. There is no fallback to running without the
// sandbox. Only state changes are logged, with a fixed bounded line, so a
// failing probe or poll loop does not repeat the message or echo API errors.
type SandboxGate struct {
	mu          sync.Mutex
	unavailable bool
	logf        func(string)
}

func (g *SandboxGate) Check(ctx context.Context, preflight func(context.Context) error) error {
	err := ErrSandboxUnavailable
	if preflight != nil && ctx.Err() == nil {
		if preflight(ctx) == nil {
			err = nil
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	report := g.logf
	if report == nil {
		report = func(s string) { log.Print(s) }
	}
	if err != nil && !g.unavailable {
		report("sandbox runtime class unavailable")
	} else if err == nil && g.unavailable {
		report("sandbox runtime class available")
	}
	g.unavailable = err != nil
	return err
}

// ErrSandboxUnavailable is the only error SandboxGate returns.
var ErrSandboxUnavailable = errors.New("sandbox runtime class unavailable")
