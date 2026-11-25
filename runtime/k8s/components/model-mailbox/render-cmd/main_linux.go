// executor-render is a bounded, offline manifest compiler. It never creates
// resources or grants execution authority. Input files remain outside source.
package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
)

func run() int {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 256*1024+1))
	if err != nil || len(raw) > 256*1024 {
		return 1
	}
	var input struct {
		Namespace    string `json:"namespace"`
		WorkerImage  string `json:"workerImage"`
		MailboxImage string `json:"mailboxImage"`
		Assignment   string `json:"assignment"`
		Profile      string `json:"profile"`
		ADevProfile  string `json:"adevProfile"`
		// RuntimeClass is optional; absent renders the default sandbox class.
		RuntimeClass string `json:"runtimeClass,omitempty"`
	}
	if authority.DecodeStrict(raw, &input) != nil {
		return 1
	}
	if input.RuntimeClass == "" {
		input.RuntimeClass = executor.DefaultRuntimeClass
	}
	workload, err := executor.Compile(input.Namespace, input.WorkerImage, input.MailboxImage, input.RuntimeClass, []byte(input.Assignment), []byte(input.Profile), []byte(input.ADevProfile), time.Now())
	if err != nil || json.NewEncoder(os.Stdout).Encode(workload) != nil {
		return 1
	}
	return 0
}
func main() {
	if len(os.Args) != 1 {
		os.Exit(1)
	}
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
