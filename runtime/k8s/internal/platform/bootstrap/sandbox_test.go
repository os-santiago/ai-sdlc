package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"
)

// classified is a preflight error with the reason and transience a classifier
// would give it.
type classified struct {
	reason  string
	unknown bool
}

func (e classified) Error() string { return "runtimeclasses.node.k8s.io \"kata\": <api detail>" }

func classify(err error) (string, bool) {
	var c classified
	if errors.As(err, &c) {
		return c.reason, c.unknown
	}
	return "", false
}

type gateClock struct{ at time.Time }

func (c *gateClock) now() time.Time { return c.at }

func newGate(grace time.Duration) (*SandboxGate, *[]string, *gateClock) {
	var lines []string
	clock := &gateClock{at: time.Date(2026, 11, 25, 3, 24, 0, 0, time.UTC)}
	g := &SandboxGate{Classify: classify, Grace: grace, logf: func(s string) { lines = append(lines, s) }, now: clock.now}
	return g, &lines, clock
}

func read(identity string, err error) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return identity, err }
}

func expectLines(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("log lines %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("log lines %q, want %q", got, want)
		}
	}
}

func TestSandboxGateFailsClosedAndLogsTransitionsOnly(t *testing.T) {
	g, lines, _ := newGate(SandboxGrace)
	missing := read("kata", classified{"not-found", false})
	for i := 0; i < 3; i++ {
		if err := g.Check(context.Background(), missing); !errors.Is(err, ErrSandboxUnavailable) {
			t.Fatal("missing sandbox class accepted")
		}
	}
	present := read("kata", nil)
	for i := 0; i < 2; i++ {
		if err := g.Check(context.Background(), present); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.Check(context.Background(), nil); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatal("absent preflight accepted")
	}
	// An unclassified error never logs its text.
	if err := g.Check(context.Background(), read("kata", errors.New("dial tcp 10.0.0.1:443: <detail>"))); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatal("unclassified failure accepted")
	}
	// An available read that names no class is refused.
	if err := g.Check(context.Background(), read("", nil)); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatal("anonymous observation accepted")
	}
	if err := g.Check(context.Background(), read("kata", classified{"overhead", false})); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatal("named refusal accepted")
	}
	expectLines(t, *lines,
		"sandbox runtime class unavailable: not-found",
		"sandbox runtime class available",
		"sandbox runtime class unavailable: preflight",
		"sandbox runtime class unavailable: overhead")
}

func TestSandboxGateNamesEveryDefiniteReason(t *testing.T) {
	for _, reason := range []string{"not-found", "no-handler", "terminating", "forbidden", "invalid", "overhead", "scheduling", "configuration"} {
		g, lines, clock := newGate(SandboxGrace)
		if err := g.Check(context.Background(), read("kata", nil)); err != nil {
			t.Fatal(err)
		}
		clock.at = clock.at.Add(time.Second)
		// A definite answer is never covered by the grace.
		if err := g.Check(context.Background(), read("kata", classified{reason, false})); !errors.Is(err, ErrSandboxUnavailable) {
			t.Fatal(reason, "accepted within grace")
		}
		expectLines(t, *lines, "sandbox runtime class available", "sandbox runtime class unavailable: "+reason)
	}
}

func TestSandboxGateGraceCoversOnlyARecentUnknownReadOfTheSameClass(t *testing.T) {
	timeout := read("kata", classified{"timeout", true})
	t.Run("unknown before any observation", func(t *testing.T) {
		g, lines, _ := newGate(SandboxGrace)
		if err := g.Check(context.Background(), timeout); !errors.Is(err, ErrSandboxUnavailable) {
			t.Fatal("unknown read accepted without an observation")
		}
		expectLines(t, *lines, "sandbox runtime class unknown: timeout")
	})
	t.Run("within and after the grace", func(t *testing.T) {
		g, lines, clock := newGate(SandboxGrace)
		observed := clock.at
		if err := g.Check(context.Background(), read("kata", nil)); err != nil {
			t.Fatal(err)
		}
		for _, offset := range []time.Duration{time.Second, 30 * time.Second, SandboxGrace} {
			clock.at = observed.Add(offset)
			if err := g.Check(context.Background(), timeout); err != nil {
				t.Fatal("unknown read within grace flipped readiness", offset)
			}
		}
		clock.at = clock.at.Add(time.Millisecond)
		if err := g.Check(context.Background(), timeout); !errors.Is(err, ErrSandboxUnavailable) {
			t.Fatal("grace outlived its bound")
		}
		// A new observation restarts the grace.
		if err := g.Check(context.Background(), read("kata", nil)); err != nil {
			t.Fatal(err)
		}
		clock.at = clock.at.Add(10 * time.Second)
		if err := g.Check(context.Background(), read("kata", classified{"server-error", true})); err != nil {
			t.Fatal("unknown read after a new observation flipped readiness")
		}
		expectLines(t, *lines,
			"sandbox runtime class available",
			"sandbox runtime class unknown: timeout; within grace",
			"sandbox runtime class unknown: timeout",
			"sandbox runtime class available",
			"sandbox runtime class unknown: server-error; within grace")
	})
	t.Run("changed class", func(t *testing.T) {
		g, _, clock := newGate(SandboxGrace)
		if err := g.Check(context.Background(), read("kata", nil)); err != nil {
			t.Fatal(err)
		}
		clock.at = clock.at.Add(time.Second)
		if err := g.Check(context.Background(), read("gvisor", classified{"timeout", true})); !errors.Is(err, ErrSandboxUnavailable) {
			t.Fatal("grace covered another class")
		}
		if err := g.Check(context.Background(), read("", classified{"timeout", true})); !errors.Is(err, ErrSandboxUnavailable) {
			t.Fatal("grace covered an unnamed read")
		}
	})
	t.Run("definite failure ends the grace", func(t *testing.T) {
		g, _, clock := newGate(SandboxGrace)
		if err := g.Check(context.Background(), read("kata", nil)); err != nil {
			t.Fatal(err)
		}
		clock.at = clock.at.Add(time.Second)
		if err := g.Check(context.Background(), read("kata", classified{"not-found", false})); !errors.Is(err, ErrSandboxUnavailable) {
			t.Fatal("missing class accepted")
		}
		clock.at = clock.at.Add(time.Second)
		if err := g.Check(context.Background(), timeout); !errors.Is(err, ErrSandboxUnavailable) {
			t.Fatal("grace survived a definite failure")
		}
	})
	t.Run("no grace configured", func(t *testing.T) {
		g, _, clock := newGate(0)
		if err := g.Check(context.Background(), read("kata", nil)); err != nil {
			t.Fatal(err)
		}
		clock.at = clock.at.Add(time.Second)
		if err := g.Check(context.Background(), timeout); !errors.Is(err, ErrSandboxUnavailable) {
			t.Fatal("unknown read accepted without a grace")
		}
	})
	t.Run("clock moved backwards", func(t *testing.T) {
		g, _, clock := newGate(SandboxGrace)
		if err := g.Check(context.Background(), read("kata", nil)); err != nil {
			t.Fatal(err)
		}
		clock.at = clock.at.Add(-time.Second)
		if err := g.Check(context.Background(), timeout); !errors.Is(err, ErrSandboxUnavailable) {
			t.Fatal("grace accepted a read before the observation")
		}
	})
}

func TestSandboxGateNamesASpentReadinessDeadlineAsTimeout(t *testing.T) {
	g, lines, _ := newGate(SandboxGrace)
	called := false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Check(ctx, func(context.Context) (string, error) { called = true; return "kata", nil }); !errors.Is(err, ErrSandboxUnavailable) || called {
		t.Fatal("canceled preflight ran or was accepted")
	}
	// A preflight whose caller's deadline expired mid-read is unknown, not
	// absent, even when its error is unclassified.
	ctx, cancel = context.WithCancel(context.Background())
	expired := func(context.Context) (string, error) { cancel(); return "kata", errors.New("context canceled") }
	if err := g.Check(ctx, expired); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatal("expired preflight accepted")
	}
	expectLines(t, *lines, "sandbox runtime class unknown: timeout")
}
