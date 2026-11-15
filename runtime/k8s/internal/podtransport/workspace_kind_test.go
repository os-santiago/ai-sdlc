package podtransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// This fixture retains one bound session from source import through observed
// worker termination. It uses synthetic model replies, no provider or database.
func TestRealWorkspaceLifecycleFixture(t *testing.T) {
	path := os.Getenv("AI_SDLC_WORKSPACE_TRANSPORT_FIXTURE")
	if path == "" {
		t.Skip("isolated kind workspace fixture required")
	}
	raw, err := os.ReadFile(path)
	var input struct {
		Config                 Config
		Binding                Binding
		SourceFile, OutputFile string
	}
	if err != nil || len(raw) > 256*1024 || json.Unmarshal(raw, &input) != nil {
		t.Fatal("private workspace fixture input")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	client, err := NewClient(input.Config)
	if err != nil {
		t.Fatal("workspace fixture client")
	}
	session, err := Bind(ctx, client, input.Binding)
	if err != nil {
		t.Fatal("workspace fixture binding")
	}
	exerciseWorkspaceLifecycle(t, ctx, session, input.Binding, input.SourceFile, input.OutputFile)
}

func exerciseWorkspaceLifecycle(t *testing.T, ctx context.Context, session *Session, binding Binding, sourceFile, outputFile string) Candidate {
	t.Helper()
	file, err := os.Open(sourceFile)
	if err != nil {
		t.Fatal("workspace fixture source")
	}
	source, err := io.ReadAll(io.LimitReader(file, workspacebundle.MaxWireBytes+1))
	file.Close()
	if err != nil || len(source) > workspacebundle.MaxWireBytes {
		t.Fatal("workspace source bound")
	}
	if session.ImportSource(ctx, source) != nil {
		t.Fatal("actual fixed source import")
	}
	if _, err = session.ExportCandidate(ctx); err == nil {
		t.Fatal("running writer exported")
	}
	if session.Start(ctx) != nil {
		t.Fatal("actual fixed start")
	}
	if session.Start(ctx) == nil {
		t.Fatal("exclusive start replay accepted")
	}
	a, err := executor.ParseAssignment(binding.Assignment, modelmailbox.Digest(binding.Assignment), time.Now())
	if err != nil {
		t.Fatal("fixture assignment")
	}
	seen := map[uint64]bool{}
	for len(seen) < 2 {
		request, err := session.ReadRequest(ctx)
		if errors.Is(err, modelmailbox.ErrPending) {
			select {
			case <-time.After(50 * time.Millisecond):
				continue
			case <-ctx.Done():
				t.Fatal("fixture model request deadline")
			}
		}
		if err != nil {
			t.Fatal("actual request read")
		}
		if seen[request.Sequence] {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		message := map[string]any{"role": "assistant", "content": "Created the isolated conformance fixture file."}
		reason := "stop"
		if len(seen) == 0 {
			arguments, _ := json.Marshal(map[string]string{"path": "AI_SDLC_EXECUTOR_FIXTURE.txt", "content": "Created by the isolated SCC executor conformance fixture.\n"})
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "workspace-fixture", "type": "function", "function": map[string]any{"name": "write_file", "arguments": string(arguments)}}}}
			reason = "tool_calls"
		}
		reply, _ := json.Marshal(map[string]any{"id": "workspace-fixture", "object": "chat.completion", "model": "fixture-model", "choices": []any{map[string]any{"index": 0, "finish_reason": reason, "message": message}}})
		request.Kind = "response"
		request.Status = 200
		request.Payload = reply
		if session.Publish(ctx, request) != nil {
			t.Fatal("actual synthetic reply publication")
		}
		seen[request.Sequence] = true
	}
	for {
		observed, err := session.inspect(ctx, true)
		if err != nil {
			t.Fatal("worker termination observation")
		}
		if observed.workerExitCode != nil {
			break
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("worker termination deadline")
		}
	}
	candidate, err := session.ExportCandidate(ctx)
	if err != nil || candidate.WorkerExitCode() != 0 {
		t.Fatal("actual stopped writer export")
	}
	exported, err := workspacebundle.Decode(candidate.Bytes())
	if err != nil || exported.SourceSHA != a.SourceSHA {
		t.Fatal("candidate source binding")
	}
	original, err := workspacebundle.Decode(source)
	if err != nil {
		t.Fatal("fixture original")
	}
	if len(exported.Files) != len(original.Files)+1 {
		t.Fatal("candidate shape")
	}
	expected := map[string]workspacebundle.File{}
	for _, f := range original.Files {
		expected[f.Path] = f
	}
	expected["AI_SDLC_EXECUTOR_FIXTURE.txt"] = workspacebundle.File{Path: "AI_SDLC_EXECUTOR_FIXTURE.txt", Mode: 0644, Data: []byte("Created by the isolated SCC executor conformance fixture.\n")}
	for _, file := range exported.Files {
		want, ok := expected[file.Path]
		if !ok || want.Mode != file.Mode || !bytes.Equal(want.Data, file.Data) {
			t.Fatal("independent candidate identity mismatch")
		}
	}
	output, err := os.OpenFile(outputFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("private candidate output")
	}
	_, err = output.Write(candidate.Bytes())
	closeErr := output.Close()
	if err != nil || closeErr != nil {
		t.Fatal("candidate output failed")
	}
	t.Log("actual fixed import/start/stopped-export passed; two synthetic model replies; no durable model broker or live execution")
	return candidate
}
