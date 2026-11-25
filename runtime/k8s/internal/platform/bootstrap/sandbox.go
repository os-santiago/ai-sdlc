package bootstrap

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// ErrSandboxUnavailable is the only error SandboxGate returns.
var ErrSandboxUnavailable = errors.New("sandbox runtime class unavailable")

// SandboxGrace bounds how long a transient (unknown) RuntimeClass read keeps a
// component ready after it last observed the same class available.
const SandboxGrace = 60 * time.Second

// SandboxGate guards the readiness of a component that creates sandboxed
// pods. Check runs the read-only sandbox RuntimeClass preflight; while it
// fails the component is not ready. Its poll loop separately reads the class
// again before every claim and refuses new work on any failure, including an
// unknown one: the gate never admits work and there is no fallback to running
// without the sandbox.
//
// The preflight returns the identity of the class(es) it read (from the
// sealed configuration), whether or not the read succeeded. Classify names a
// failure with a bounded reason and whether it is transient (the class is
// unknown: the API server timed out or answered 429 or 5xx). A transient
// failure keeps the component ready only while the same identity was observed
// available within Grace; a definite failure (not found, no handler,
// terminating, refused overhead or scheduling), a changed identity or an
// expired grace makes it not ready at once.
//
// Only state changes are logged, with a fixed bounded line:
//
//	sandbox runtime class available
//	sandbox runtime class unavailable: <reason>
//	sandbox runtime class unknown: <reason>
//	sandbox runtime class unknown: <reason>; within grace
//
// Reasons come only from Classify (an unclassified error is "preflight"), so
// no API error text is ever logged.
type SandboxGate struct {
	Classify func(error) (reason string, unknown bool)
	Grace    time.Duration

	mu        sync.Mutex
	line      string
	identity  string    // identity of the last available observation
	available time.Time // when it was observed
	now       func() time.Time
	logf      func(string)
}

func (g *SandboxGate) Check(ctx context.Context, preflight func(context.Context) (string, error)) error {
	clock := g.now
	if clock == nil {
		clock = time.Now
	}
	identity, reason, unknown := "", "preflight", false
	switch {
	case preflight == nil:
	case ctx.Err() != nil:
		// The readiness deadline was spent before the class could be read.
		reason, unknown = "timeout", true
	default:
		var err error
		identity, err = preflight(ctx)
		named, transient := "", false
		if err != nil && g.Classify != nil {
			named, transient = g.Classify(err)
		}
		switch {
		case err == nil:
			reason = ""
		case named != "":
			reason, unknown = named, transient
		case ctx.Err() != nil:
			reason, unknown = "timeout", true
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := clock()
	line, ready := "sandbox runtime class available", true
	switch {
	case reason == "" && identity != "":
		g.identity, g.available = identity, now
	case reason == "":
		// An available read must name what it observed.
		line, ready = "sandbox runtime class unavailable: preflight", false
		g.identity = ""
	case unknown && identity != "" && identity == g.identity && g.Grace > 0 && !now.Before(g.available) && now.Sub(g.available) <= g.Grace:
		line = "sandbox runtime class unknown: " + reason + "; within grace"
	case unknown:
		line, ready = "sandbox runtime class unknown: "+reason, false
	default:
		line, ready = "sandbox runtime class unavailable: "+reason, false
		// A definite failure ends any grace.
		g.identity = ""
	}
	if line != g.line {
		report := g.logf
		if report == nil {
			report = func(s string) { log.Print(s) }
		}
		report(line)
	}
	g.line = line
	if !ready {
		return ErrSandboxUnavailable
	}
	return nil
}
