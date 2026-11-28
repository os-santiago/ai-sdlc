package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/generatortools"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "tools-mcp" {
		os.Exit(toolsMCP(os.Args[2:]))
	}
	if len(os.Args) != 3 || os.Args[1] != "--assignment-sha256" {
		os.Exit(1)
	}
	started := time.Now()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The absolute bound also covers unexpected blocking I/O before SCC is
	// started. It derives from the reviewed profile (980 s at the default
	// 600 s budget, 1,280 s at 900 s, always 40 s before the Pod deadline);
	// reading that profile is itself bounded by the default 980 s.
	bound := make(chan int, 1)
	go func() { bound <- executor.ProjectedProcessSeconds(executor.InputDirectory, os.Args[2], time.Now()) }()
	var seconds int
	select {
	case seconds = <-bound:
	case <-time.After(executor.DefaultProcessSeconds * time.Second):
		os.Exit(124)
	}
	ctx, cancel := context.WithDeadline(ctx, started.Add(time.Duration(seconds)*time.Second))
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- executor.Run(ctx, os.Args[2]) }()
	select {
	case code := <-done:
		os.Exit(code)
	case <-ctx.Done():
		os.Exit(124)
	}
}

// toolsMCP serves the generator tools MCP server that SCC starts from its
// trusted configuration (executor.SCCConfiguration). Its spec and bounds come
// from that configuration, which the launcher verified against the reviewed
// profile; it reaches nothing but the loopback mailbox.
func toolsMCP(args []string) int {
	flags := flag.NewFlagSet("tools-mcp", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	rawSpec := flags.String("spec", "", "generator tool spec")
	wait := flags.Int("wait-seconds", 0, "window for one tool call")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *wait < executor.MinToolSeconds || *wait > executor.MaxToolSeconds {
		return 2
	}
	var spec generatortools.Spec
	if json.Unmarshal([]byte(*rawSpec), &spec) != nil || spec.Validate() != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	candidate := generatortools.GitCandidate("/usr/bin/git", executor.WorkspaceDirectory+"/repository", executor.WorkspaceDirectory+"/tmp")
	if generatortools.Serve(ctx, os.Stdin, os.Stdout, spec, candidate, generatortools.MailboxRelay(time.Duration(*wait)*time.Second)) != nil {
		return 1
	}
	return 0
}
