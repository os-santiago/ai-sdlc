package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestSandboxGateFailsClosedAndLogsTransitionsOnly(t *testing.T) {
	var lines []string
	g := &SandboxGate{logf: func(s string) { lines = append(lines, s) }}
	missing := func(context.Context) error {
		return errors.New("runtimeclasses.node.k8s.io \"kata\" not found: <api detail>")
	}
	present := func(context.Context) error { return nil }
	for i := 0; i < 3; i++ {
		if err := g.Check(context.Background(), missing); !errors.Is(err, ErrSandboxUnavailable) {
			t.Fatal("missing sandbox class accepted")
		}
	}
	if err := g.Check(context.Background(), present); err != nil {
		t.Fatal(err)
	}
	if err := g.Check(context.Background(), present); err != nil {
		t.Fatal(err)
	}
	if err := g.Check(context.Background(), nil); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatal("absent preflight accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Check(ctx, present); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatal("canceled preflight accepted")
	}
	named := errors.New("sandbox runtime class overhead exceeds quota headroom")
	g.Known = []error{named}
	if err := g.Check(context.Background(), func(context.Context) error { return fmt.Errorf("wrapped: %w", named) }); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatal("named failure accepted")
	}
	want := []string{"sandbox runtime class unavailable", "sandbox runtime class available", "sandbox runtime class unavailable", "sandbox runtime class overhead exceeds quota headroom"}
	if len(lines) != len(want) {
		t.Fatal("unexpected log lines", lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatal("unbounded or repeated log line", lines)
		}
	}
}
