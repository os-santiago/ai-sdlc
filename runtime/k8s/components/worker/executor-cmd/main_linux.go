package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
)

func main() {
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
