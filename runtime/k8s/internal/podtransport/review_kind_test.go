package podtransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
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
	budget := strings.HasPrefix(in.Scenario, "budget-")
	verdict := strings.TrimPrefix(in.Scenario, "budget-")
	switch in.Scenario {
	case "approve", "request_changes", "implicit", "mutation", "budget-approve", "budget-request_changes", "budget-extra-tool":
	default:
		t.Fatal("fixture scenario")
	}
	finalCall, readCalls := uint64(2), 1
	if budget {
		finalCall, readCalls = 3, 8
	}
	extraTool := in.Scenario == "budget-extra-tool"
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
		// Fixture diagnostics contain only synthetic workload state, never Pod
		// configuration, credentials, generated text or arbitrary log content.
		if raw, _, e := resources.request(ctx, http.MethodGet, "pods", resources.pod.Name, nil, false); e == nil {
			var p struct {
				Status struct {
					Phase             string
					ContainerStatuses []struct {
						Name, ImageID string
						State         map[string]struct {
							Reason   string
							ExitCode int
						}
					}
				}
			}
			if json.Unmarshal(raw, &p) == nil {
				t.Logf("synthetic fixture status: %+v", p.Status)
			}
		}
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
	evidence := []store.ReviewModelEvidence{}
	guardRejected := false
	finalToolsDisabled := false
	budgetRejected := false
	for {
		code, done, e := session.WorkerExit(ctx)
		if e != nil {
			t.Fatal("worker observation")
		}
		if done {
			if extraTool && code == 22 {
				raw, helperCode, e := session.exchangeFixed(ctx, helperOperation{command: []string{"/workspace-transfer", "review", "--pod-uid", session.identity.PodUID, "--source-sha", session.sourceSHA}, outputLimit: 65536, frames: 1024, timeout: 5 * time.Second}, nil)
				var summary struct {
					ExitReason string          `json:"exit_reason"`
					Tools      int             `json:"tool_calls_total"`
					Iterations int             `json:"iterations"`
					Review     json.RawMessage `json:"review"`
				}
				if e != nil || helperCode != 0 || json.Unmarshal(raw, &summary) != nil || summary.ExitReason != "budget_exceeded" || summary.Tools != 8 || summary.Iterations != 3 || (len(summary.Review) != 0 && string(summary.Review) != "null") {
					t.Fatal("extra tool was not rejected at the exact budget")
				}
				budgetRejected = true
				break
			}
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
		if budget && request.Sequence == finalCall {
			var completion struct {
				Tools  []json.RawMessage `json:"tools"`
				Choice string            `json:"tool_choice"`
			}
			if json.Unmarshal(request.Payload, &completion) != nil || completion.Tools == nil || len(completion.Tools) != 0 || completion.Choice != "none" {
				t.Fatal("final reviewer request did not disable tools")
			}
			finalToolsDisabled = true
		}
		message := map[string]any{"role": "assistant", "content": "Inspected the source.\nVERDICT: " + verdict}
		if in.Scenario == "budget-request_changes" {
			message["content"] = "The review is blocked due to insufficient evidence.\nVERDICT: request_changes"
		}
		reason := "stop"
		if request.Sequence < finalCall || extraTool {
			function := "read_file"
			arguments, _ := json.Marshal(map[string]string{"path": "REVIEW_FIXTURE.txt"})
			if in.Scenario == "mutation" {
				function = "run_shell"
				arguments, _ = json.Marshal(map[string]string{"command": "printf changed > REVIEW_FIXTURE.txt"})
			}
			count := 1
			if budget && request.Sequence < finalCall {
				count = 4
			}
			calls := []any{}
			for i := 0; i < count; i++ {
				calls = append(calls, map[string]any{"id": fmt.Sprintf("review-%d-%d", request.Sequence, i), "type": "function", "function": map[string]any{"name": function, "arguments": string(arguments)}})
			}
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": calls}
			reason = "tool_calls"
		} else if in.Scenario == "implicit" {
			message["content"] = "Looks good to me."
		} else if in.Scenario == "mutation" {
			message["content"] = "Inspected the source.\nVERDICT: approve"
		}
		payload, _ := json.Marshal(map[string]any{"id": "fixture", "object": "chat.completion", "model": "fixture-model", "choices": []any{map[string]any{"index": 0, "finish_reason": reason, "message": message}}})
		observation, guardErr := modelbroker.ObserveReviewResponse(payload, "fixture-model")
		if guardErr != nil {
			guardRejected = true
		} else {
			raw, _ := json.Marshal(observation)
			evidence = append(evidence, store.ReviewModelEvidence{Sequence: int(request.Sequence), RequestDigest: request.RequestSHA256, ResponseDigest: modelmailbox.Digest(payload), InputBytes: len(request.Payload), OutputBytes: len(payload), Observation: raw})
		}
		// This isolated transport fixture deliberately delivers negative replies
		// to test SCC as well. The production review broker rejects them BEFORE
		// delivery; its PostgreSQL/HTTP failure tests cover that boundary.
		request.Kind, request.Status, request.Payload = "response", 200, payload
		if session.Publish(ctx, request) != nil {
			t.Fatal("synthetic response delivery")
		}
		seen = request.Sequence
	}
	if seen != finalCall || budget && !finalToolsDisabled {
		t.Fatal("review did not exercise inspection and final verdict")
	}
	if extraTool {
		if !budgetRejected {
			t.Fatal("reviewer dispatched an extra tool")
		}
		if _, err := session.ExportReview(ctx); err == nil {
			t.Fatal("exhausted reviewer exported approval")
		}
		report, _ := json.Marshal(map[string]any{"scope": "isolated-readonly-reviewer-component", "scenario": in.Scenario, "realSCC": true, "realKubernetes": true, "syntheticModel": true, "syntheticIssue": true, "modelCalls": seen, "toolCalls": 8, "finalToolsDisabled": true, "extraToolRejected": true, "strictReviewAccepted": false, "publicationAuthorized": false, "autonomousE2E": false})
		if os.WriteFile(in.ReportFile, report, 0600) != nil {
			t.Fatal("fixture report")
		}
		return
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
	accepted := verdict == "approve" || verdict == "request_changes"
	if accepted && (err != nil || review.Result().Verdict != verdict || review.Result().ReadCalls != readCalls) {
		t.Fatal("explicit review rejected", err)
	}
	if accepted && (guardRejected || modelbroker.MatchReviewSummary(review.Result(), evidence) != nil) {
		t.Fatal("actual SCC summary did not match independent provider observations")
	}
	if !accepted && !guardRejected {
		t.Fatal("guard accepted unsafe review response")
	}
	if !accepted && err == nil {
		t.Fatal("unsafe review accepted")
	}
	report, _ := json.Marshal(map[string]any{"scope": "isolated-readonly-reviewer-component", "scenario": in.Scenario, "realSCC": true, "realKubernetes": true, "syntheticModel": true, "syntheticIssue": true, "sourceUnchanged": true, "modelCalls": seen, "toolCalls": readCalls, "finalToolsDisabled": finalToolsDisabled, "strictReviewAccepted": accepted, "providerObservationsMatched": accepted, "unsafeResponseRejectedByGuard": guardRejected, "publicationAuthorized": false, "autonomousE2E": false, "podUID": podUID})
	if os.WriteFile(in.ReportFile, report, 0600) != nil {
		t.Fatal("fixture report")
	}
}
