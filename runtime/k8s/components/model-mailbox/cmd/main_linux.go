package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
)

func run(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("expected serve, read or respond")
	}
	mode := args[0]
	if mode != "serve" && mode != "read" && mode != "respond" {
		return errors.New("unsupported mailbox mode")
	}
	flags := flag.NewFlagSet("model-mailbox", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	attempt := flags.String("attempt", "", "trusted attempt identity")
	uid := flags.String("pod-uid", "", "expected Pod UID")
	model := flags.String("model", "", "reviewed model, required with token cap")
	maxTokens := flags.Int("max-tokens", 0, "reviewed per-request token cap")
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
		return errors.New("invalid mailbox arguments")
	}
	if (mode != "serve" && (*model != "" || *maxTokens != 0)) || ((*model == "") != (*maxTokens == 0)) {
		return errors.New("invalid mailbox request limits")
	}
	id := modelmailbox.Identity{Attempt: *attempt, PodUID: *uid}
	if id.Validate() != nil {
		return errors.New("invalid mailbox identity")
	}
	// This projected file is mounted read-only by the immutable Pod template.
	// Its Kubernetes-managed symlink is expected; no path is accepted from SCC.
	f, err := os.Open("/var/run/ai-sdlc/pod/uid")
	if err != nil {
		return errors.New("Pod UID projection is required")
	}
	observed, readErr := io.ReadAll(io.LimitReader(f, 129))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || len(observed) > 128 || strings.TrimSpace(string(observed)) != id.PodUID {
		return errors.New("Pod UID does not match assigned executor")
	}
	store, err := modelmailbox.OpenStore("/bridge")
	if err != nil {
		return err
	}
	defer store.Close()
	switch mode {
	case "serve":
		var server *modelmailbox.Server
		var err error
		if *model != "" {
			server, err = modelmailbox.NewRestrictedServer(store, id, 60*time.Second, modelmailbox.RequestLimits{Model: *model, MaxTokens: *maxTokens})
		} else {
			server, err = modelmailbox.NewServer(store, id, 60*time.Second)
		}
		if err != nil {
			return err
		}
		return server.Listen(ctx)
	case "read":
		request, err := store.ReadRequest(id)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(request)
	case "respond":
		raw, err := io.ReadAll(io.LimitReader(input, modelmailbox.MaxEnvelopeBytes+1))
		if err != nil {
			return errors.New("cannot read bounded response")
		}
		response, err := modelmailbox.Decode(raw)
		if err != nil {
			return err
		}
		return store.WriteResponse(id, response)
	}
	return errors.New("unsupported mailbox mode")
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	args := os.Args[1:]
	invoke := func() error { return run(ctx, args, os.Stdin, os.Stdout) }
	var err error
	if len(args) > 0 && args[0] == "serve" {
		err = invoke()
	} else {
		deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		err = bounded(deadline, invoke)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		os.Exit(124)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, modelmailbox.ErrPending) {
			os.Exit(3)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// The main process exits on deadline even if a pods/exec peer stalls stdin or
// stdout. A bounded byte count alone cannot bound a blocking pipe operation.
func bounded(ctx context.Context, invoke func() error) error {
	done := make(chan error, 1)
	go func() { done <- invoke() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
