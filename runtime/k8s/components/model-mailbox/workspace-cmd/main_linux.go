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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repositorytests"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/reviewresult"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func run(args []string, input io.Reader, output io.Writer, uidPath, parent string) error {
	if len(args) == 0 || (args[0] != "import" && args[0] != "export" && args[0] != "review" && args[0] != "ready" && args[0] != "hold") {
		return workspacebundle.ErrInvalid
	}
	flags := flag.NewFlagSet("workspace-transfer", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	uid := flags.String("pod-uid", "", "expected assigned Pod UID")
	source := flags.String("source-sha", "", "expected source commit")
	digest := flags.String("digest", "", "expected import bundle SHA256")
	tree := flags.String("tree-sha256", "", "expected imported tree SHA256")
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 || (modelmailbox.Identity{Attempt: "workspace-transfer", PodUID: *uid}).Validate() != nil {
		return workspacebundle.ErrInvalid
	}
	switch args[0] {
	case "hold":
		// hold only proves the pod identity; it takes no tree arguments.
		if *source != "" || *digest != "" || *tree != "" {
			return workspacebundle.ErrInvalid
		}
	case "ready":
		if !workspacebundle.ValidSourceSHA(*source) || !workspacebundle.ValidDigest(*tree) || *digest != "" {
			return workspacebundle.ErrInvalid
		}
	default:
		if !workspacebundle.ValidSourceSHA(*source) || *tree != "" ||
			(args[0] == "import" && !workspacebundle.ValidDigest(*digest)) || (args[0] != "import" && *digest != "") {
			return workspacebundle.ErrInvalid
		}
	}
	f, e := os.Open(uidPath)
	if e != nil {
		return workspacebundle.ErrInvalid
	}
	observed, e := io.ReadAll(io.LimitReader(f, 129))
	closeErr := f.Close()
	if e != nil || closeErr != nil || len(observed) > 128 || strings.TrimSpace(string(observed)) != *uid {
		return workspacebundle.ErrInvalid
	}
	switch args[0] {
	case "hold":
		return nil
	case "ready":
		// The controller calls ready only after it checked the import receipt;
		// the marker releases the test container of this pod and tree. The tree
		// is checked again here, before any repository code can run.
		if repositorytests.VerifyTree(parent, *source, *tree) != nil {
			return workspacebundle.ErrInvalid
		}
		return repositorytests.PublishReady(parent, repositorytests.Ready{Version: repositorytests.ReadyVersion, PodUID: *uid, SourceSHA: *source, TreeSHA256: *tree})
	}
	if args[0] == "import" {
		b, e := workspacebundle.ReadVerified(input, *digest, *source)
		if e != nil {
			return e
		}
		if workspacebundle.Import(parent, b) != nil {
			return workspacebundle.ErrInvalid
		}
		return json.NewEncoder(output).Encode(map[string]any{"imported": true, "files": len(b.Files), "sourceSHA": *source, "bundleSHA256": *digest})
	}
	if args[0] == "review" {
		raw, e := reviewresult.Read(filepath.Join(filepath.Dir(parent), "review-state"))
		if e != nil {
			return workspacebundle.ErrInvalid
		}
		_, e = output.Write(raw)
		return e
	}
	b, e := workspacebundle.Export(parent, *source)
	if e != nil {
		return e
	}
	raw, e := workspacebundle.Encode(b)
	if e != nil {
		return e
	}
	_, e = output.Write(raw)
	return e
}
func bounded(ctx context.Context, invoke func() error) error {
	done := make(chan error, 1)
	go func() { done <- invoke() }()
	select {
	case e := <-done:
		return e
	case <-ctx.Done():
		return ctx.Err()
	}
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) >= 2 && os.Args[1] == "hold" {
		// The idle exec target of a repository-tests pod: no listener, no
		// bridge, only the identity check, then wait for termination.
		if run(os.Args[1:], nil, io.Discard, "/var/run/ai-sdlc/pod/uid", "/workspaces") != nil {
			fmt.Fprintln(os.Stderr, "workspace transfer rejected")
			os.Exit(1)
		}
		<-ctx.Done()
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	err := bounded(ctx, func() error {
		e := run(os.Args[1:], os.Stdin, os.Stdout, "/var/run/ai-sdlc/pod/uid", "/workspaces")
		if e != nil {
			fmt.Fprintln(os.Stderr, "workspace transfer rejected")
		}
		return e
	})
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		os.Exit(124)
	}
	if err != nil {
		// A refused export exits with its bounded reason (3 to 7, see
		// workspacebundle.ExportReason); any other failure stays 1.
		if reason := workspacebundle.RefusalReason(err); reason != 0 {
			os.Exit(int(reason))
		}
		os.Exit(1)
	}
}
