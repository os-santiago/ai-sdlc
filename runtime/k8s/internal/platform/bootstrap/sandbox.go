package bootstrap

import (
	"context"
	"errors"
	"log"
	"sync"
)

// ErrSandboxUnavailable is the only error SandboxGate returns.
var ErrSandboxUnavailable = errors.New("sandbox runtime class unavailable")

// SandboxGate guards the readiness of a component that creates sandboxed
// pods. Check runs the read-only sandbox RuntimeClass preflight; while it
// fails the component is not ready (its poll loop separately refuses to claim
// work). There is no fallback to running without the sandbox. Only state
// changes are logged, with a fixed bounded line: Known lists the preflight's
// own named errors whose message may be logged; anything else (including API
// error text) is reported as "sandbox runtime class unavailable".
type SandboxGate struct {
	Known  []error
	mu     sync.Mutex
	reason string
	logf   func(string)
}

func (g *SandboxGate) Check(ctx context.Context, preflight func(context.Context) error) error {
	reason := ErrSandboxUnavailable.Error()
	if preflight != nil && ctx.Err() == nil {
		if err := preflight(ctx); err == nil {
			reason = ""
		} else {
			for _, known := range g.Known {
				if errors.Is(err, known) {
					reason = known.Error()
				}
			}
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	report := g.logf
	if report == nil {
		report = func(s string) { log.Print(s) }
	}
	if reason != g.reason {
		if reason == "" {
			report("sandbox runtime class available")
		} else {
			report(reason)
		}
	}
	g.reason = reason
	if reason != "" {
		return ErrSandboxUnavailable
	}
	return nil
}
