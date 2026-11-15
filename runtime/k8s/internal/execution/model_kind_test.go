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
		Config                                                  podtransport.Config
		Placement                                               Placement
		Profile, ADev                                           []byte
		SourceFile, OutputFile, ReportFile, PlanFile, ReadyFile string
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
	snapshot.Issue.Body = "Synthetic component fixture; not a live issue.\n## Acceptance criteria\n- Create AI_SDLC_EXECUTOR_FIXTURE.txt containing the requested fixture text.\n- Preserve every original source file.\n"
	snapshot.Revision = snapshot.ContentRevision()
	f.seedReadinessSnapshot(t, snapshot)
	if _, err = f.accept(f.envelope(t, f.command)); err != nil {
		t.Fatal("signed fixture delivery")
	}
	claim, err := Claim(context.Background(), f.db, f.readyPolicy, f.command.ID())
	if err != nil {
		t.Fatal("signed fixture claim")
	}
	_, err = f.Admin.Pool.Exec(`GRANT SELECT,INSERT ON execution_lifecycles,execution_effects,execution_models,model_budgets,model_allocations,model_calls TO aisdlc_worker;
 GRANT UPDATE(phase,config_uid,pod_uid,updated_at) ON execution_lifecycles TO aisdlc_worker;
 GRANT UPDATE(result,completed_at) ON execution_effects TO aisdlc_worker;
 GRANT UPDATE(status,updated_at) ON work_items TO aisdlc_worker;
 GRANT UPDATE(reserved_calls,reserved_input_bytes,reserved_output_bytes,state,stop_reason) ON model_budgets TO aisdlc_worker;
 GRANT UPDATE(reserved_calls,reserved_input_bytes,reserved_output_bytes) ON model_allocations TO aisdlc_worker;
 GRANT UPDATE ON model_calls TO aisdlc_worker`)
	if err != nil {
		t.Fatal("fixture-only grants")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 140*time.Second)
	defer cancel()
	load := func(context.Context) (PreparationInputs, error) {
		return PreparationInputs{Policy: f.Material.Policy, Intake: f.Config, Profile: input.Profile, ADev: input.ADev}, nil
	}
	prepared, err := prepare(ctx, f.db, load, claim, func(context.Context, string, string) (workspacebundle.Bundle, error) { return bundle, nil })
	if err != nil {
		t.Fatal("actual preparation")
	}
	l, err := OpenLifecycle(ctx, f.db, func(ctx context.Context) (PreparationInputs, Placement, error) {
		in, e := load(ctx)
		return in, input.Placement, e
	}, prepared)
	if err != nil {
		t.Fatal("actual lifecycle ownership")
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
	plan, _ := json.Marshal(map[string]string{"namespace": l.durable.Binding.Namespace, "pod": l.durable.Binding.PodName, "config": l.durable.Binding.ConfigName})
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
	resources, err := podtransport.PlanResources(client, podtransport.Binding{Namespace: input.Placement.Namespace, WorkerImage: input.Placement.WorkerImage, MailboxImage: input.Placement.MailboxImage, Assignment: prepared.Assignment(), Profile: prepared.Profile(), ADev: prepared.ADev()})
	if err != nil {
		t.Fatal("fixed resources")
	}
	configUID, podUID := "", ""
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 25*time.Second)
		defer stop()
		if podUID != "" {
			if e := resources.DeletePod(cleanup, podUID); e != nil {
				t.Error("owned Pod cleanup")
			}
		}
		if configUID != "" {
			if e := resources.DeleteConfig(cleanup, configUID); e != nil {
				t.Error("owned ConfigMap cleanup")
			}
		}
	}()
	if err = l.RunEffect(ctx, "config", func(ctx context.Context, _ EffectInput) (store.LifecycleResult, error) {
		configUID, err = resources.CreateConfig(ctx)
		return store.LifecycleResult{UID: configUID}, err
	}); err != nil {
		t.Fatal("durable config effect")
	}
	if err = l.RunEffect(ctx, "pod", func(ctx context.Context, in EffectInput) (store.LifecycleResult, error) {
		podUID, err = resources.CreatePod(ctx, in.ConfigUID)
		return store.LifecycleResult{UID: podUID}, err
	}); err != nil {
		t.Fatal("durable Pod effect")
	}
	session, err := resources.WaitRunning(ctx, podUID)
	if err != nil {
		t.Fatal("original Pod readiness")
	}
	if err = l.RunEffect(ctx, "source", func(ctx context.Context, in EffectInput) (store.LifecycleResult, error) {
		e := session.ImportSource(ctx, in.Source)
		return store.LifecycleResult{UID: in.PodUID, Digest: workspacebundle.Digest(in.Source), Bytes: len(in.Source)}, e
	}); err != nil {
		t.Fatal("durable source effect")
	}
	scope, err := l.ActivateModel(ctx, func(context.Context) (ModelPolicy, error) {
		return ModelPolicy{Limits: store.ModelLimits{Calls: 4, InputBytes: 512 * 1024, OutputBytes: 128 * 1024}, ResponseLimit: 32 * 1024}, nil
	})
	if err != nil {
		t.Fatal("atomic model activation")
	}
	if err = l.RunEffect(ctx, "start", func(ctx context.Context, in EffectInput) (store.LifecycleResult, error) {
		return store.LifecycleResult{UID: in.PodUID}, session.Start(ctx)
	}); err != nil {
		t.Fatal("durable start effect")
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
		if n == 1 {
			args, _ := json.Marshal(map[string]string{"path": "AI_SDLC_EXECUTOR_FIXTURE.txt", "content": "Created by the isolated SCC executor conformance fixture.\n"})
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "lifecycle-fixture", "type": "function", "function": map[string]any{"name": "write_file", "arguments": string(args)}}}}
			reason = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "lifecycle-fixture", "object": "chat.completion", "model": "fixture-model", "choices": []any{map[string]any{"index": 0, "finish_reason": reason, "message": message}}})
	}))
	defer server.Close()
	provider, err := modelbroker.NewProvider(modelbroker.ProviderConfig{Endpoint: server.URL + "/v1/chat/completions", BearerToken: strings.Repeat("x", 32), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal("synthetic HTTP model fixture")
	}
	dispatcher := modelbroker.Dispatcher{DB: f.db, Provider: provider, Mailbox: session, Authority: l}
	seen := map[uint64]bool{}
	for len(seen) < 2 {
		req, e := session.ReadRequest(ctx)
		if errors.Is(e, modelmailbox.ErrPending) {
			time.Sleep(25 * time.Millisecond)
			continue
		}
		if e != nil {
			t.Fatal("actual mailbox read")
		}
		if seen[req.Sequence] {
			time.Sleep(25 * time.Millisecond)
			continue
		}
		result, e := dispatcher.Deliver(ctx, scope.AllocationID, prepared.Profile(), req)
		if e != nil || !result.DispatchAttempted || !result.HandoffConfirmed {
			t.Fatal("lifecycle-authorized model handoff")
		}
		seen[req.Sequence] = true
	}
	for {
		code, exited, e := session.WorkerExit(ctx)
		if e != nil {
			t.Fatal("bound worker observation")
		}
		if exited {
			if code != 0 {
				t.Fatal("fixture worker exit")
			}
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	var candidate podtransport.Candidate
	if err = l.RunEffect(ctx, "export", func(ctx context.Context, in EffectInput) (store.LifecycleResult, error) {
		candidate, err = session.ExportCandidate(ctx)
		code := candidate.WorkerExitCode()
		return store.LifecycleResult{UID: in.PodUID, Digest: workspacebundle.Digest(candidate.Bytes()), Bytes: len(candidate.Bytes()), ExitCode: &code}, err
	}); err != nil {
		t.Fatal("durable candidate export")
	}
	if _, err = l.Authorize(ctx, scope); err == nil {
		t.Fatal("post-export model authority")
	}
	if calls.Load() != 2 || f.Count(t, `SELECT count(*) FROM model_calls WHERE state='completed' AND response_handed_off_at IS NOT NULL`) != 2 || f.Count(t, `SELECT count(*) FROM execution_effects WHERE result IS NOT NULL`) != 5 {
		t.Fatal("durable effect/handoff evidence")
	}
	writePrivate(input.OutputFile, candidate.Bytes())
	result, _ := json.Marshal(map[string]any{"scope": "isolated-composed-component-fixture", "syntheticAuthority": true, "syntheticModelReplies": true, "liveWorkflowAdvanced": false, "modelCalls": calls.Load(), "durableModelHandoffs": 2, "durableEffects": 5, "originalPodUID": podUID, "originalConfigUID": configUID, "workerExitCode": candidate.WorkerExitCode(), "candidateBytes": len(candidate.Bytes()), "workItemStatus": "running", "lifecyclePhase": "export-ready", "postExportAuthorityDenied": true})
	writePrivate(input.ReportFile, result)
}
