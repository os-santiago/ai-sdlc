package podtransport

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Explicit isolated component fixture. Synthetic replies exercise the real SCC
// reviewer, fixed helpers and gVisor mounts; no live issue/controller is invoked.
func TestRealReadonlyReviewerFixture(t *testing.T) {
	path := os.Getenv("AI_SDLC_REVIEWER_FIXTURE")
	if path == "" {
		t.Skip("explicit disposable reviewer fixture required")
	}
	var in struct {
		Config                           Config
		Binding                          Binding
		SourceFile, ReportFile, Scenario string
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 256*1024 || json.Unmarshal(raw, &in) != nil {
		t.Fatal("fixture input")
	}
	if in.Scenario != "approve" && in.Scenario != "request_changes" && in.Scenario != "implicit" && in.Scenario != "mutation" {
		t.Fatal("fixture scenario")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	client, err := NewClient(in.Config)
	if err != nil {
		t.Fatal("fixture client")
	}
	resources, err := PlanResources(client, in.Binding)
	if err != nil {
		t.Fatal("fixture template")
	}
	configUID, err := resources.CreateConfig(ctx)
	if err != nil {
		t.Fatal("fixture config")
	}
	var podUID string
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if podUID != "" && resources.DeletePod(cleanup, podUID) != nil {
			t.Error("fixture pod cleanup")
		}
		if resources.DeleteConfig(cleanup, configUID) != nil {
			t.Error("fixture config cleanup")
		}
	}()
	podUID, err = resources.CreatePod(ctx, configUID)
	if err != nil {
		t.Fatal("fixture pod")
	}
	session, err := resources.WaitRunning(ctx, podUID)
	if err != nil {
		t.Fatal("fixture readiness")
	}
	source, err := os.ReadFile(in.SourceFile)
	if err != nil || len(source) > workspacebundle.MaxWireBytes {
		t.Fatal("fixture source")
	}
	if session.ImportSource(ctx, source) != nil {
		t.Fatal("fixed source import")
	}
	if _, err := session.ExportReview(ctx); err == nil {
		t.Fatal("running reviewer exported")
	}
	if session.Start(ctx) != nil {
		t.Fatal("fixed review start")
	}
	seen := uint64(0)
	for {
		code, done, e := session.WorkerExit(ctx)
		if e != nil {
			t.Fatal("worker observation")
		}
		if done {
			if code != 10 {
				raw, _, _ := session.exchangeFixed(ctx, helperOperation{command: []string{"/workspace-transfer", "review", "--pod-uid", session.identity.PodUID, "--source-sha", session.sourceSHA}, outputLimit: 65536, frames: 1024, timeout: 5 * time.Second}, nil)
				var summary struct {
					ExitReason string `json:"exit_reason"`
					Resolution string `json:"resolution"`
					Review     any    `json:"review"`
					Phases     any    `json:"phases"`
				}
				if json.Unmarshal(raw, &summary) == nil {
					t.Logf("synthetic fixture summary: %+v", summary)
				}
				t.Fatalf("reviewer exit=%d calls=%d", code, seen)
			}
			break
		}
		request, e := session.ReadRequest(ctx)
		if errors.Is(e, modelmailbox.ErrPending) || errors.Is(e, ErrWorkerExited) {
			select {
			case <-ctx.Done():
				t.Fatal("review deadline")
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}
		if e != nil {
			t.Fatal("review mailbox read")
		}
		if request.Sequence == seen {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if request.Sequence != seen+1 || request.Sequence > 3 {
			t.Fatal("unexpected review request")
		}
		message := map[string]any{"role": "assistant", "content": "Inspected the source.\nVERDICT: " + in.Scenario}
		reason := "stop"
		if request.Sequence == 1 {
			function := "read_file"
			arguments, _ := json.Marshal(map[string]string{"path": "REVIEW_FIXTURE.txt"})
			if in.Scenario == "mutation" {
				function = "run_shell"
				arguments, _ = json.Marshal(map[string]string{"command": "printf changed > REVIEW_FIXTURE.txt"})
			}
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "review-fixture", "type": "function", "function": map[string]any{"name": function, "arguments": string(arguments)}}}}
			reason = "tool_calls"
		} else if in.Scenario == "implicit" {
			message["content"] = "Looks good to me."
		} else if in.Scenario == "mutation" {
			message["content"] = "Inspected the source.\nVERDICT: approve"
		}
		payload, _ := json.Marshal(map[string]any{"id": "fixture", "object": "chat.completion", "model": "fixture-model", "choices": []any{map[string]any{"index": 0, "finish_reason": reason, "message": message}}})
		request.Kind, request.Status, request.Payload = "response", 200, payload
		if session.Publish(ctx, request) != nil {
			t.Fatal("synthetic response delivery")
		}
		seen = request.Sequence
	}
	if seen != 2 {
		t.Fatal("review did not exercise inspection and final verdict")
	}
	exported, err := session.ExportCandidate(ctx)
	if err != nil {
		t.Fatal("source export")
	}
	bundle, err := workspacebundle.Decode(exported.Bytes())
	if err != nil {
		t.Fatal("source decode")
	}
	tree, err := bundle.TreeDigest()
	a, parseErr := executor.ParseAssignment(in.Binding.Assignment, workspacebundle.Digest(in.Binding.Assignment), time.Now())
	if err != nil || parseErr != nil || tree != a.TreeSHA256 {
		t.Fatal("reviewer changed source")
	}
	review, err := session.ExportReview(ctx)
	accepted := in.Scenario == "approve" || in.Scenario == "request_changes"
	if accepted && (err != nil || review.Result().Verdict != in.Scenario || review.Result().ReadCalls != 1) {
		t.Fatal("explicit review rejected", err)
	}
	if !accepted && err == nil {
		t.Fatal("unsafe review accepted")
	}
	report, _ := json.Marshal(map[string]any{"scope": "isolated-readonly-reviewer-component", "scenario": in.Scenario, "realSCC": true, "realKubernetes": true, "syntheticModel": true, "syntheticIssue": true, "sourceUnchanged": true, "modelCalls": seen, "strictReviewAccepted": accepted, "publicationAuthorized": false, "autonomousE2E": false, "podUID": podUID})
	if os.WriteFile(in.ReportFile, report, 0600) != nil {
		t.Fatal("fixture report")
	}
}
