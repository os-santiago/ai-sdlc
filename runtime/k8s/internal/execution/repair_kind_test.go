package execution_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publicationflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repairflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/reviewflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/sourcefetch"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// The controller fixture seeds predecessor history. The composition fixture
// uses a synthetic captured predecessor but actual behavior and review twice.
// Both use synthetic model replies and never establish application E2E.
func TestRealConnectedRepairControllerFixture(t *testing.T)  { runRepairKind(t, false) }
func TestRealConnectedRepairCompositionFixture(t *testing.T) { runRepairKind(t, true) }
func runRepairKind(t *testing.T, composition bool) {
	path := os.Getenv("AI_SDLC_CANDIDATE_FIXTURE")
	if path == "" {
		t.Skip("explicit isolated repair Kubernetes fixture required")
	}
	var in struct {
		Config                           podtransport.Config
		Runtime                          execution.RuntimeConfig
		ReportFile, Scenario             string
		SourceFile                       string
		Changes                          map[string]string
		VerificationConfig, ReviewConfig podtransport.Config
		VerificationImage                string
	}
	raw, e := os.ReadFile(path)
	if e != nil || len(raw) > 256*1024 || json.Unmarshal(raw, &in) != nil || (in.Scenario != "repair" && in.Scenario != "repair-composition") {
		t.Fatal("private repair fixture input")
	}
	var f execution.RepairFixtureForTest
	var reviewConfig reviewflow.Configuration
	var reviewCalls atomic.Int32
	var reviewedRepair atomic.Bool
	readPath, writePath, writeContent := "candidate-fixture.txt", "repair-fixture.txt", "repaired\n"
	if composition {
		readPath = "tests/js/utils-locale-fallback.test.js"
		writePath = readPath
		writeContent = in.Changes[readPath] + "\ntest('Bounded repair regression: missing document locale uses browser locale', () => {\n  for (const lang of [null, undefined]) {\n    assert.equal(load(lang, 'es-CL').formatDate(raw), date.toLocaleDateString('es-CL', defaults));\n  }\n});\n"
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 48) {
				w.WriteHeader(401)
				return
			}
			if r.Method == "GET" && r.URL.Path == "/readyz" {
				io.WriteString(w, `{"ready":true}`)
				return
			}
			b, err := io.ReadAll(io.LimitReader(r.Body, 131073))
			if err != nil || len(b) > 131072 || !json.Valid(b) {
				w.WriteHeader(400)
				return
			}
			if strings.Contains(string(b), "Bounded repair regression") {
				reviewedRepair.Store(true)
			}
			n := reviewCalls.Add(1)
			message := map[string]any{"role": "assistant", "content": "Inspected the repaired regression test.\nVERDICT: approve"}
			if n == 2 {
				message["content"] = "Add a boundary regression test.\nVERDICT: request_changes"
			}
			finish := "stop"
			if n == 1 || n == 3 {
				args, _ := json.Marshal(map[string]string{"path": readPath})
				message["content"] = nil
				message["tool_calls"] = []any{map[string]any{"id": "repair-review", "type": "function", "function": map[string]string{"name": "read_file", "arguments": string(args)}}}
				finish = "tool_calls"
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"model": "codex-cli", "choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}}})
		}))
		defer provider.Close()
		adev := []byte(in.Runtime.ADev)
		profile, _ := json.Marshal(map[string]any{"version": "ai-sdlc.reviewer-profile/v1", "model": "codex-cli", "maxSteps": 4, "maxSeconds": 120, "maxTokens": 2048, "aDevSHA256": authority.Digest(adev)})
		reviewConfig = reviewflow.Configuration{Namespace: "ai-sdlc-reviewers", WorkerImage: in.Runtime.Placement.WorkerImage, MailboxImage: in.Runtime.Placement.MailboxImage, Profile: profile, ADev: adev, Model: modelbroker.ProviderConfig{Endpoint: provider.URL + "/v1/chat/completions", BearerToken: strings.Repeat("x", 48), AllowLoopbackHTTP: true}}
		sourceRaw, err := os.ReadFile(in.SourceFile)
		if err != nil || len(sourceRaw) > workspacebundle.MaxWireBytes {
			t.Fatal("pinned source input")
		}
		source, err := workspacebundle.Decode(sourceRaw)
		if err != nil {
			t.Fatal("source bundle", err)
		}
		f = execution.NewKubeRepairCompositionForTest(t, in.Runtime.Placement, adev, source, in.Changes)
		passwords := map[string]string{}
		for role := range f.Identities {
			passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
		}
		if err = f.Admin.ProvisionRolesForCapabilities(context.Background(), passwords, "verification", true, true); err != nil {
			t.Fatal("publication fixture roles", err)
		}
	} else {
		f = execution.NewKubeRepairFixtureForTest(t, in.Runtime.Placement, []byte(in.Runtime.ADev))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 420*time.Second)
	defer cancel()
	// The actual verification and review stages fetch the pinned source from
	// the forge. Unauthenticated, shared CI runners hit the per-address rate
	// limit and the stage stops at "source". CI's read-only identity stays in
	// the trusted test process; it is never serialized into fixture controls,
	// Pod specs, model prompts or reports.
	if token := os.Getenv("AI_SDLC_TEST_SOURCE_TOKEN"); token != "" {
		scoped, err := sourcefetch.WithReadCredential(ctx, "os-santiago/homedir", token)
		if err != nil {
			t.Fatal("invalid fixture source credential")
		}
		ctx = scoped
	}
	var original string
	if e = f.Admin.Pool.QueryRow(`SELECT row_to_json(b)::text FROM model_budgets b`).Scan(&original); e != nil {
		t.Fatal(e)
	}
	if composition {
		policy := func() (authority.Policy, intake.Config, string, error) {
			return f.Policy, f.Config, f.Command.ProfileSHA256, nil
		}
		verifierClient, err := podtransport.NewClient(in.VerificationConfig)
		if err != nil {
			t.Fatal(err)
		}
		if state, err := (verificationflow.Runtime{DB: f.Verifier, Load: policy, Client: verifierClient, Image: in.VerificationImage, RuntimeClass: in.Runtime.Placement.SandboxRuntimeClass()}).PollOnce(ctx); err != nil || state != "behavior-verified" {
			t.Fatal("actual predecessor behavior", state, err)
		}
		reviewClient, err := podtransport.NewClient(in.ReviewConfig)
		if err != nil {
			t.Fatal(err)
		}
		if state, err := (reviewflow.Runtime{DB: f.Verifier, Policy: policy, Load: func(context.Context) (reviewflow.Configuration, error) { return reviewConfig, nil }, Client: reviewClient}).PollOnce(ctx); err == nil || state != "stopped" {
			t.Fatal("actual predecessor rejection", state, err)
		}
		var reviewID, state, reason, cleanup string
		if err := f.Admin.Pool.QueryRow(`SELECT id,state,reason,cleanup_state FROM review_runs WHERE candidate_id=$1`, f.Command.ID()).Scan(&reviewID, &state, &reason, &cleanup); err != nil || state != "failed" || reason != "request-changes" || cleanup != "complete" || reviewCalls.Load() != 2 {
			t.Fatal("actual rejection journal", state, reason, cleanup, err)
		}
		f.Review, err = f.Verifier.ReadReview(ctx, reviewID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if sent, e := repairflow.Handoff(ctx, f.Verifier, repairLoad(f, "verifier")); e != nil || !sent {
		t.Fatal("repair handoff", sent, e)
	}
	if _, e = relayRepair(t, f, "repair-candidate", repairLoad(f, "orchestrator")); e != nil {
		t.Fatal("repair inbox", e)
	}
	if _, e = relayRepair(t, f, "execute-work", nil); e != nil {
		t.Fatal("child inbox", e)
	}
	var child string
	if e = f.Admin.Pool.QueryRow(`SELECT child_request_id FROM execution_repairs`).Scan(&child); e != nil {
		t.Fatal(e)
	}
	prepared := f.PrepareOnly(t, child)
	var calls atomic.Int32
	var feedbackSeen, parentSeen atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/readyz" {
			io.WriteString(w, `{"ready":true}`)
			return
		}
		b, e := io.ReadAll(io.LimitReader(r.Body, 131073))
		if e != nil || len(b) > 131072 || !json.Valid(b) {
			w.WriteHeader(400)
			return
		}
		n := calls.Add(1)
		if strings.Contains(string(b), "Add a boundary regression test.") {
			feedbackSeen.Store(true)
		}
		if strings.Contains(string(b), "unvalidated") || (composition && strings.Contains(string(b), "utils-locale-fallback")) {
			parentSeen.Store(true)
		}
		message := map[string]any{"role": "assistant", "content": "Repair fixture complete."}
		finish := "stop"
		if n < 3 {
			argsRaw, _ := json.Marshal(map[string]string{"path": readPath})
			name, args := "read_file", string(argsRaw)
			if n == 2 {
				argsRaw, _ = json.Marshal(map[string]string{"path": writePath, "content": writeContent})
				name, args = "write_file", string(argsRaw)
			}
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"id": "repair-fixture", "type": "function", "function": map[string]string{"name": name, "arguments": args}}}
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"model": "codex-cli", "choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}}})
	}))
	defer server.Close()
	provider, e := modelbroker.NewProvider(modelbroker.ProviderConfig{Endpoint: server.URL + "/v1/chat/completions", BearerToken: strings.Repeat("x", 48), AllowLoopbackHTTP: true})
	if e != nil {
		t.Fatal(e)
	}
	client, e := podtransport.NewClient(in.Config)
	if e != nil {
		t.Fatal(e)
	}
	record, e := execution.RunController(ctx, f.Worker, f.LoadLifecycle, prepared, client, podtransport.RuntimeClassAdmission{}, provider, func(context.Context) (execution.ModelPolicy, error) { return f.Runtime.Model, nil })
	if e != nil {
		t.Fatal("actual child controller", execution.DiagnosticStage(e), "calls", calls.Load())
	}
	if record.RequestID != child || record.Changes != 2 || record.ModelCalls != 3 || calls.Load() != 3 || !feedbackSeen.Load() || !parentSeen.Load() {
		t.Fatal("repair did not preserve parent and consume private feedback", record.Changes, record.ModelCalls, calls.Load(), feedbackSeen.Load(), parentSeen.Load())
	}
	var after, state, reason, cleanup string
	if e = f.Admin.Pool.QueryRow(`SELECT row_to_json(b)::text FROM model_budgets b`).Scan(&after); e != nil || after != original {
		t.Fatal("parent budget changed", e)
	}
	if e = f.Admin.Pool.QueryRow(`SELECT r.state,r.stop_reason,l.cleanup_state FROM execution_requests r JOIN execution_lifecycles l ON l.request_id=r.id WHERE r.id=$1`, child).Scan(&state, &reason, &cleanup); e != nil || state != "stopped" || reason != "candidate-captured" || cleanup != "complete" {
		t.Fatal("child not retired", state, reason, cleanup, e)
	}
	publicationQueued := false
	if composition {
		// Fixture-only stand-in for the installed intake's unchanged-content poll
		// (every PollSeconds). Without it the single seeded observation ages past
		// 2*PollSeconds on slow runners; freshness checks themselves are unchanged.
		observeUnchangedIssue := func() {
			if _, err := f.Admin.Pool.Exec(`UPDATE intake_observations SET observed_at=clock_timestamp(); UPDATE intake_cursors SET watermark=clock_timestamp()`); err != nil {
				t.Fatal("fixture intake poll", err)
			}
		}
		policy := func() (authority.Policy, intake.Config, string, error) {
			return f.Policy, f.Config, f.Command.ProfileSHA256, nil
		}
		observeUnchangedIssue()
		if sent, err := execution.HandoffCandidate(ctx, f.Worker, func() (authority.Policy, authority.Identity, intake.Config, string, error) {
			return f.Policy, f.Identities["worker"], f.Config, f.Command.ProfileSHA256, nil
		}); err != nil || !sent {
			t.Fatal("child verification handoff", sent, err)
		}
		if _, err := relayRepair(t, f, "verify-artifact", nil); err != nil {
			t.Fatal("child verification inbox", err)
		}
		verifierClient, err := podtransport.NewClient(in.VerificationConfig)
		if err != nil {
			t.Fatal(err)
		}
		observeUnchangedIssue()
		if state, err := (verificationflow.Runtime{DB: f.Verifier, Load: policy, Client: verifierClient, Image: in.VerificationImage, RuntimeClass: in.Runtime.Placement.SandboxRuntimeClass()}).PollOnce(ctx); err != nil || state != "behavior-verified" {
			t.Fatal("actual repaired behavior", state, err)
		}
		reviewClient, err := podtransport.NewClient(in.ReviewConfig)
		if err != nil {
			t.Fatal(err)
		}
		observeUnchangedIssue()
		if state, err := (reviewflow.Runtime{DB: f.Verifier, Policy: policy, Load: func(context.Context) (reviewflow.Configuration, error) { return reviewConfig, nil }, Client: reviewClient}).PollOnce(ctx); err != nil || state != "review-passed" {
			t.Fatal("actual repaired review", state, err)
		}
		if reviewCalls.Load() != 4 || !reviewedRepair.Load() {
			t.Fatal("independent reviewer did not read repaired tree", reviewCalls.Load(), reviewedRepair.Load())
		}
		expected := func() (publicationflow.ReviewPolicy, error) {
			return publicationflow.ReviewPolicy{Profile: authority.Digest(reviewConfig.Profile), Configuration: reviewConfig.Digest()}, nil
		}
		identity := func() (authority.Policy, authority.Identity, error) { return f.Policy, f.Identities["verifier"], nil }
		observeUnchangedIssue()
		if sent, err := publicationflow.Handoff(ctx, f.Verifier, policy, expected, identity); err != nil || !sent {
			t.Fatal("normal publication handoff", sent, err)
		}
		if sent, err := publicationflow.Handoff(ctx, f.Verifier, policy, expected, identity); err != nil || sent {
			t.Fatal("duplicate publication handoff", sent, err)
		}
		var count int
		if err := f.Admin.Pool.QueryRow(`SELECT count(*) FROM authorized_outbox WHERE operation='publish-candidate'`).Scan(&count); err != nil || count != 1 {
			t.Fatal("publication outbox", count, err)
		}
		publicationQueued = true
	}
	report, _ := json.Marshal(map[string]any{"scope": "isolated-connected-repair-controller", "syntheticParentHistory": true, "syntheticInitialCandidate": true, "realInitialRejection": composition, "syntheticSource": !composition, "syntheticModel": true, "realPostgres": true, "realKubernetes": true, "realSCC": true, "signedRepairHandoff": true, "signedOrchestratorInbox": true, "signedChildInbox": true, "parentBudgetUnchanged": true, "privateFeedbackConsumed": true, "rejectedTreeImported": true, "cumulativeChanges": record.Changes, "childModelCalls": record.ModelCalls, "candidateDeltaSHA256": record.DeltaSHA256, "assignmentSHA256": authority.Digest(prepared.Assignment()), "childCleanupComplete": true, "autonomousE2E": false, "realChildBehaviorAndReview": composition, "independentReviewCalls": reviewCalls.Load(), "normalPublicationHandoff": publicationQueued, "forgeWrites": false})
	if e = os.WriteFile(in.ReportFile, report, 0600); e != nil {
		t.Fatal("report")
	}
}
