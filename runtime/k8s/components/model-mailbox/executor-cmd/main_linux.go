// executor-control is a fixed, Pod-local start helper. It cannot create an
// assignment, invoke a provider or authorize a workflow transition.
package main

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
)

func run() int {
	if len(os.Args) != 5 || os.Args[1] != "start" {
		return 1
	}
	f, err := os.Open("/var/run/ai-sdlc/pod/uid")
	if err != nil {
		return 1
	}
	uid, err := io.ReadAll(io.LimitReader(f, 129))
	f.Close()
	if err != nil || len(uid) > 128 || strings.TrimSpace(string(uid)) != os.Args[2] {
		return 1
	}
	if executor.PublishStart("/workspaces", executor.Start{Version: "ai-sdlc.executor-start/v1", PodUID: os.Args[2], AssignmentSHA256: os.Args[3], TreeSHA256: os.Args[4]}) != nil {
		return 1
	}
	return 0
}
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- run() }()
	select {
	case code := <-done:
		os.Exit(code)
	case <-ctx.Done():
		os.Exit(124)
	}
}
