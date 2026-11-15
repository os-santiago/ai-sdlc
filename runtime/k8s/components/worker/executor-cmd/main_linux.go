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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Absolute bound also covers unexpected blocking I/O before SCC is started.
	ctx, cancel := context.WithTimeout(ctx, 980*time.Second)
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
