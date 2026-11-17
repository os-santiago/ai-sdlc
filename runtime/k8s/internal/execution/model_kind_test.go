package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Only an explicitly selected disposable database/namespace may run this test.
// Authority and HTTP model replies are synthetic; Kubernetes, SCC, PostgreSQL
// ownership and the independently checked pinned Homedir source are real.
func TestRealLifecycleModelFixture(t *testing.T) {
	path := os.Getenv("AI_SDLC_LIFECYCLE_FIXTURE")
	if path == "" {
		t.Skip("isolated lifecycle Kubernetes fixture required")
	}
	var input struct {
		Scenario                                                           string
		Endpoint, Token                                                    string
		Config                                                             podtransport.Config
		Placement                                                          Placement
		Profile, ADev                                                      []byte
		SourceFile, OutputFile, ReportFile, PlanFile, ReadyFile, DeltaFile string
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 256*1024 || json.Unmarshal(raw, &input) != nil {
		t.Fatal("private lifecycle fixture input")
	}
	sourceFile, err := os.Open(input.SourceFile)
	if err != nil {
		t.Fatal("source fixture")
	}
	sourceRaw, err := io.ReadAll(io.LimitReader(sourceFile, workspacebundle.MaxWireBytes+1))
	sourceFile.Close()
	if err != nil || len(sourceRaw) > workspacebundle.MaxWireBytes {
		t.Fatal("source bound")
	}
	bundle, err := workspacebundle.Decode(sourceRaw)
	if err != nil {
		t.Fatal("source bundle")
	}
	f := newFixture(t)
	f.Material.Policy.Repository = "os-santiago/homedir"
	f.Config.Repository = f.Material.Policy.Repository
	f.command.Repository = f.Material.Policy.Repository
	f.command.AuthorityRevision = f.Material.Policy.Revision()
	f.command.ProfileSHA256 = workspacebundle.Digest(input.Profile)
	snapshot := f.Snapshot(1, true)
	snapshot.BaseSHA = bundle.SourceSHA
	snapshot.Issue.Title = "Isolated lifecycle conformance fixture"
	snapshot.Issue.Body = "Synthetic component fixture; not a live issue.\n## Acceptance criteria\n- Create AI_SDLC_EXECUTOR_FIXTURE.txt containing exactly: Created by the isolated SCC executor conformance fixture. End with a newline.\n- Preserve every original source file.\n"
	snapshot.Revision = snapshot.ContentRevision()
	f.seedReadinessSnapshot(t, snapshot)
	if _, err = f.accept(f.envelope(t, f.command)); err != nil {
		t.Fatal("signed fixture delivery")
	}
	claim, err := Claim(context.Background(), f.db, f.readyPolicy, f.command.ID())
	if err != nil {
		t.Fatal("signed fixture claim")
	}
	_, err = f.Admin.Pool.Exec(`GRANT SELECT,INSERT ON execution_lifecycles,execution_effects,execution_models,execution_candidates,model_budgets,model_allocations,model_calls TO aisdlc_worker;
 GRANT UPDATE(phase,config_uid,pod_uid,updated_at,cleanup_state) ON execution_lifecycles TO aisdlc_worker;
 GRANT UPDATE(result,completed_at) ON execution_effects TO aisdlc_worker;
 GRANT SELECT ON execution_failures TO aisdlc_worker;
 GRANT INSERT(request_id,pod_uid,container_id,exit_code) ON execution_failures TO aisdlc_worker;
 GRANT UPDATE(status,updated_at) ON work_items TO aisdlc_worker;
 GRANT UPDATE(reserved_calls,reserved_input_bytes,reserved_output_bytes,state,stop_reason) ON model_budgets TO aisdlc_worker;
 GRANT UPDATE(reserved_calls,reserved_input_bytes,reserved_output_bytes) ON model_allocations TO aisdlc_worker;
 GRANT UPDATE ON model_calls TO aisdlc_worker`)
	if err != nil {
		t.Fatal("fixture-only grants")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 260*time.Second)
	defer cancel()
	load := func(context.Context) (PreparationInputs, error) {
		return PreparationInputs{Policy: f.Material.Policy, Intake: f.Config, Profile: input.Profile, ADev: input.ADev}, nil
	}
	prepared, err := prepare(ctx, f.db, load, claim, func(context.Context, string, string) (workspacebundle.Bundle, error) { return bundle, nil })
	if err != nil {
		t.Fatal("actual preparation")
	}
	loader := func(ctx context.Context) (PreparationInputs, Placement, error) {
		in, e := load(ctx)
		return in, input.Placement, e
	}
	workload, err := executor.Compile(input.Placement.Namespace, input.Placement.WorkerImage, input.Placement.MailboxImage, prepared.Assignment(), prepared.Profile(), prepared.ADev(), time.Now())
	if err != nil {
		t.Fatal("fixed workload plan")
	}
	writePrivate := func(path string, value []byte) {
		file, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			t.Fatal("private fixture output")
		}
		_, e = file.Write(value)
		closed := file.Close()
		if e != nil || closed != nil {
			t.Fatal("fixture output write")
		}
	}
	plan, _ := json.Marshal(map[string]string{"namespace": input.Placement.Namespace, "pod": workload.Pod["metadata"].(map[string]any)["name"].(string), "config": workload.ConfigMap["metadata"].(map[string]any)["name"].(string)})
	writePrivate(input.PlanFile, plan)
	readyDeadline := time.Now().Add(20 * time.Second)
	for {
		if _, err = os.Stat(input.ReadyFile); err == nil {
			break
		}
		if time.Now().After(readyDeadline) || ctx.Err() != nil {
			t.Fatal("fixture RBAC bootstrap deadline")
		}
		time.Sleep(25 * time.Millisecond)
	}
	client, err := podtransport.NewClient(input.Config)
	if err != nil {
		t.Fatal("fixture Kubernetes client")
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, e := io.Copy(io.Discard, io.LimitReader(r.Body, modelmailbox.MaxRequestBytes+1)); e != nil {
			w.WriteHeader(400)
			return
		}
		n := calls.Add(1)
		message := map[string]any{"role": "assistant", "content": "Created the isolated conformance fixture file."}
		reason := "stop"
		if input.Scenario == "provider-failure" {
			w.WriteHeader(503)
			return
		}
		if input.Scenario == "cancel-provider" {
			cancel()
			w.WriteHeader(503)
			return
		}
		if n == 1 && !strings.HasPrefix(input.Scenario, "no-change") {
			args, _ := json.Marshal(map[string]string{"path": "AI_SDLC_EXECUTOR_FIXTURE.txt", "content": "Created by the isolated SCC executor conformance fixture.\n"})
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "lifecycle-fixture", "type": "function", "function": map[string]any{"name": "write_file", "arguments": string(args)}}}}
			reason = "tool_calls"
		}
		if (input.Scenario == "three-turns" && n == 2) || (strings.HasPrefix(input.Scenario, "no-change") && n == 1) {
			path := "AI_SDLC_EXECUTOR_FIXTURE.txt"
			if strings.HasPrefix(input.Scenario, "no-change") {
				path = "README.md"
			}
			args, _ := json.Marshal(map[string]string{"path": path})
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "inspect-fixture", "type": "function", "function": map[string]any{"name": "read_file", "arguments": string(args)}}}}
			reason = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "lifecycle-fixture", "object": "chat.completion", "model": "fixture-model", "choices": []any{map[string]any{"index": 0, "finish_reason": reason, "message": message}}})
	}))
	defer server.Close()
	endpoint, token := server.URL+"/v1/chat/completions", strings.Repeat("x", 32)
	if input.Scenario == "real-model" {
		endpoint, token = input.Endpoint, input.Token
	}
	provider, err := modelbroker.NewProvider(modelbroker.ProviderConfig{Endpoint: endpoint, BearerToken: token, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal("synthetic HTTP model fixture")
	}
	limits := store.ModelLimits{Calls: 4, InputBytes: 512 * 1024, OutputBytes: 128 * 1024}
	if input.Scenario == "real-model" {
		limits = store.ModelLimits{Calls: 8, InputBytes: 1024 * 1024, OutputBytes: 512 * 1024}
	}
	if input.Scenario == "no-change-store-failure" {
		if _, e := f.Admin.Pool.Exec(`REVOKE INSERT(request_id,pod_uid,container_id,exit_code) ON execution_failures FROM aisdlc_worker`); e != nil {
			t.Fatal(e)
		}
	}
	captured, err := RunController(ctx, f.db, loader, prepared, client, provider, func(context.Context) (ModelPolicy, error) {
		return ModelPolicy{Limits: limits, ResponseLimit: 32 * 1024}, nil
	})
	if input.Scenario == "provider-failure" || input.Scenario == "cancel-provider" || strings.HasPrefix(input.Scenario, "no-change") {
		if err == nil || captured.RequestID != "" || f.Count(t, `SELECT count(*) FROM execution_candidates`) != 0 || f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='stopped'`) != 1 {
			t.Fatal("failed controller retained authority/candidate", err)
		}
		expectedCalls := int32(1)
		if strings.HasPrefix(input.Scenario, "no-change") {
			expectedCalls = calls.Load()
			if expectedCalls < 1 || expectedCalls > 4 || f.Count(t, `SELECT count(*) FROM model_calls WHERE state='completed' AND response_handed_off_at IS NOT NULL`) != int(expectedCalls) {
				t.Fatal("no-change exchange exceeded budget or left uncertain calls")
			}
			var failure ControllerError
			if !errors.As(err, &failure) || failure.Stage != "worker-exit" {
				t.Fatal("unexpected no-change rejection", err)
			}
			code, recorded := WorkerFailureDiagnostic(err)
			expectedRecorded := input.Scenario != "no-change-store-failure"
			expectedCount := 0
			if expectedRecorded {
				expectedCount = 1
			}
			if code != 10 || recorded != expectedRecorded || f.Count(t, `SELECT count(*) FROM execution_failures f JOIN execution_lifecycles l ON l.request_id=f.request_id WHERE f.pod_uid=l.pod_uid AND f.exit_code=10 AND l.cleanup_state='complete'`) != expectedCount {
				t.Fatal("original nonzero exit did not survive containment cleanup")
			}
			if f.Count(t, `SELECT count(*) FROM execution_lifecycles WHERE cleanup_state='complete'`) != 1 {
				t.Fatal("diagnostic persistence prevented containment cleanup")
			}
		}
		if calls.Load() != expectedCalls {
			t.Fatal("unexpected failed scenario calls", calls.Load())
		}
		code, recorded := WorkerFailureDiagnostic(err)
		result, _ := json.Marshal(map[string]any{"scope": "isolated-composed-component-fixture", "scenario": input.Scenario, "syntheticAuthority": true, "syntheticModelReplies": true, "liveWorkflowAdvanced": false, "controller": "RunController", "candidateCaptured": false, "candidateValidated": false, "diagnosticStage": DiagnosticStage(err), "claimStopped": true, "modelCalls": calls.Load(), "workerNonzeroExitObserved": strings.HasPrefix(input.Scenario, "no-change"), "workerExitCode": code, "failureRecorded": recorded})
		writePrivate(input.ReportFile, result)
		return
	}
	if err != nil {
		t.Fatal("controller composition", DiagnosticStage(err), "cleanupPending", CleanupPending(err))
	}
	expectedCalls := int32(2)
	if input.Scenario == "three-turns" {
		expectedCalls = 3
	}
	if input.Scenario == "real-model" {
		expectedCalls = int32(f.Count(t, `SELECT count(*) FROM model_calls`))
		if expectedCalls < 1 || expectedCalls > 8 {
			t.Fatal("real model call budget")
		}
		calls.Store(expectedCalls)
	}
	if captured.Changes != 1 || captured.ModelCalls != int(expectedCalls) || calls.Load() != expectedCalls || f.Count(t, `SELECT count(*) FROM model_calls WHERE state='completed' AND response_handed_off_at IS NOT NULL`) != int(expectedCalls) || f.Count(t, `SELECT count(*) FROM execution_effects WHERE result IS NOT NULL`) != 5 {
		t.Fatal("controller durable evidence")
	}
	var delta []byte
	if err = f.Admin.Pool.QueryRow(`SELECT delta FROM execution_candidates WHERE request_id=$1`, captured.RequestID).Scan(&delta); err != nil {
		t.Fatal("fixture candidate read")
	}
	writePrivate(input.DeltaFile, delta)
	decoded, err := candidate.Decode(delta)
	if err != nil {
		t.Fatal("delta decode")
	}
	reconstructed, err := candidate.Apply(bundle, decoded)
	if err != nil {
		t.Fatal("delta reconstruction")
	}
	exported, err := workspacebundle.Encode(reconstructed)
	if err != nil {
		t.Fatal("bundle encoding")
	}
	writePrivate(input.OutputFile, exported)
	result, _ := json.Marshal(map[string]any{"scope": "isolated-composed-component-fixture", "syntheticAuthority": true, "syntheticModelReplies": input.Scenario != "real-model", "liveWorkflowAdvanced": false, "modelCalls": calls.Load(), "durableModelHandoffs": expectedCalls, "durableEffects": 5, "originalPodUID": captured.PodUID, "workerExitCode": 0, "candidateBytes": len(exported), "controller": "RunController", "workItemStatus": "running", "lifecyclePhase": "candidate-captured", "candidateDeltaSHA256": captured.DeltaSHA256, "candidateTreeSHA256": captured.CandidateTreeSHA256, "candidateDeltaBytes": len(delta), "candidateValidated": false, "modelBudgetSealed": true})
	writePrivate(input.ReportFile, result)
}
