package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func TestFixedHelperRoundTripAndIdentity(t *testing.T) {
	parent := t.TempDir()
	uid := "11111111-1111-4111-8111-111111111111"
	uidPath := filepath.Join(t.TempDir(), "uid")
	if e := os.WriteFile(uidPath, []byte(uid), 0444); e != nil {
		t.Fatal(e)
	}
	b := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: strings.Repeat("a", 40), Files: []workspacebundle.File{{Path: "file", Mode: 0644, Data: []byte("bounded")}}}
	raw, e := workspacebundle.Encode(b)
	if e != nil {
		t.Fatal(e)
	}
	args := []string{"import", "--pod-uid", uid, "--source-sha", b.SourceSHA, "--digest", workspacebundle.Digest(raw)}
	var result bytes.Buffer
	if e = run(args, bytes.NewReader(raw), &result, uidPath, parent); e != nil {
		t.Fatal(e)
	}
	result.Reset()
	if e = run([]string{"export", "--pod-uid", uid, "--source-sha", b.SourceSHA}, nil, &result, uidPath, parent); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(result.Bytes(), raw) {
		t.Fatal("helper changed bytes")
	}
	// A refused import (here over the existing repository) still consumes
	// its stdin, so an exec client writing it never blocks.
	input := bytes.NewReader(raw)
	if run(args, input, io.Discard, uidPath, parent) == nil || input.Len() != 0 {
		t.Fatal("refused import left stdin unread", input.Len())
	}
	args[2] = "22222222-2222-4222-8222-222222222222"
	if run(args, bytes.NewReader(raw), io.Discard, uidPath, t.TempDir()) == nil {
		t.Fatal("wrong UID accepted")
	}
	args[2] = uid
	args = append(args, "--path", "/tmp")
	if run(args, bytes.NewReader(raw), io.Discard, uidPath, parent) == nil {
		t.Fatal("caller path accepted")
	}
}
func TestHelperDeadlinesBoundBlockedPipes(t *testing.T) {
	for _, mode := range []string{"input", "output"} {
		t.Run(mode, func(t *testing.T) {
			r, w := io.Pipe()
			defer r.Close()
			defer w.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			e := bounded(ctx, func() error {
				if mode == "input" {
					_, e := io.ReadAll(r)
					return e
				}
				_, e := w.Write([]byte("blocked"))
				return e
			})
			if !errors.Is(e, context.DeadlineExceeded) {
				t.Fatalf("got %v", e)
			}
		})
	}
}

func TestFixedReviewHelperRejectsForeignUIDAndCallerPaths(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "workspaces")
	runDir := filepath.Join(root, "review-state", "run")
	if e := os.MkdirAll(runDir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(runDir, "summary.json"), []byte(`{"advisory":true}`), 0600); e != nil {
		t.Fatal(e)
	}
	uid := "11111111-1111-4111-8111-111111111111"
	uidPath := filepath.Join(root, "uid")
	if e := os.WriteFile(uidPath, []byte(uid), 0600); e != nil {
		t.Fatal(e)
	}
	args := []string{"review", "--pod-uid", uid, "--source-sha", strings.Repeat("a", 40)}
	var result bytes.Buffer
	if e := run(args, nil, &result, uidPath, parent); e != nil || result.String() != `{"advisory":true}` {
		t.Fatal(e)
	}
	for _, suffix := range [][]string{{"--path", "/etc/passwd"}, {"--digest", strings.Repeat("b", 64)}} {
		if run(append(append([]string{}, args...), suffix...), nil, io.Discard, uidPath, parent) == nil {
			t.Fatal("review accepts extra authority")
		}
	}
	args[2] = "22222222-2222-4222-8222-222222222222"
	if run(args, nil, io.Discard, uidPath, parent) == nil {
		t.Fatal("foreign reviewer accepted")
	}
}

func TestReadyAndHoldModes(t *testing.T) {
	parent := t.TempDir()
	uid := "11111111-1111-4111-8111-111111111111"
	uidPath := filepath.Join(t.TempDir(), "uid")
	if e := os.WriteFile(uidPath, []byte(uid), 0444); e != nil {
		t.Fatal(e)
	}
	b := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: strings.Repeat("a", 40), Files: []workspacebundle.File{{Path: "file", Mode: 0644, Data: []byte("bounded")}}}
	if e := workspacebundle.Import(parent, b); e != nil {
		t.Fatal(e)
	}
	tree, _ := b.TreeDigest()
	if run([]string{"hold", "--pod-uid", uid}, nil, io.Discard, uidPath, parent) != nil {
		t.Fatal("hold with the right identity refused")
	}
	for _, bad := range [][]string{
		{"hold", "--pod-uid", "22222222-2222-4222-8222-222222222222"},
		{"hold", "--pod-uid", uid, "--source-sha", b.SourceSHA},
		{"ready", "--pod-uid", uid, "--source-sha", b.SourceSHA},
		{"ready", "--pod-uid", uid, "--source-sha", b.SourceSHA, "--tree-sha256", strings.Repeat("b", 64)},
		{"export", "--pod-uid", uid, "--source-sha", b.SourceSHA, "--tree-sha256", tree},
	} {
		if run(bad, nil, io.Discard, uidPath, parent) == nil {
			t.Fatal("accepted", bad)
		}
	}
	if _, e := os.Stat(filepath.Join(parent, "ready.json")); e == nil {
		t.Fatal("a refused ready published a marker")
	}
	ready := []string{"ready", "--pod-uid", uid, "--source-sha", b.SourceSHA, "--tree-sha256", tree}
	if e := run(ready, nil, io.Discard, uidPath, parent); e != nil {
		t.Fatal(e)
	}
	if run(ready, nil, io.Discard, uidPath, parent) == nil {
		t.Fatal("a second ready replaced the marker")
	}
}
