package execution

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/sourcefetch"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Real Kubernetes/Postgres/SCC/public pinned Homedir source; synthetic issue
// and model responses. This fixture never advances the live product workflow.
func TestRealCandidateRuntimeFixture(t *testing.T) {
	path := os.Getenv("AI_SDLC_CANDIDATE_FIXTURE")
	if path == "" {
		t.Skip("explicit disposable runtime fixture required")
	}
	var in struct {
		Config                          podtransport.Config
		Runtime                         RuntimeConfig
		SourceSHA, ReportFile, Scenario string
		Changes                         map[string]string
		VerificationDriver              string
		VerificationConfig              podtransport.Config
		VerificationScenario            string
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 256*1024 || json.Unmarshal(raw, &in) != nil {
		t.Fatal("fixture configuration")
	}
	t.Setenv("AI_SDLC_RUNTIME_MODE", "candidate")
	f := newFixture(t)
	candidatePrivileges(t, f)
	f.Material.Policy.Repository = "os-santiago/homedir"
	var grants []authority.Grant
	for _, g := range f.Material.Policy.Grants {
		if g.Operation == "admit-issue" || g.Operation == "plan-admission" || g.Operation == "execute-work" {
			grants = append(grants, g)
		}
	}
	if in.Scenario == "verification" {
		t.Setenv("AI_SDLC_RUNTIME_MODE", "verification")
		if os.Getenv("AI_SDLC_PUBLICATION_FIXTURE") == "1" {
			t.Setenv("AI_SDLC_INDEPENDENT_REVIEW", "true")
			t.Setenv("AI_SDLC_PUBLICATION", "true")
		}
		for _, g := range f.Material.Policy.Grants {
			if g.Operation == "verify-artifact" || (g.Operation == "publish-candidate" && os.Getenv("AI_SDLC_PUBLICATION_FIXTURE") == "1") {
				grants = append(grants, g)
			}
		}
	}
	if in.Scenario == "verification" {
		passwords := map[string]string{}
		for role := range f.Material.Identities {
			passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
		}
		if e := f.Admin.ProvisionRolesForMode(context.Background(), passwords, "verification"); e != nil {
			t.Fatal(e)
		}
	}
	f.Material.Policy.Grants = grants
	f.Config.Repository = f.Material.Policy.Repository
	f.command.Repository = f.Material.Policy.Repository
	f.command.AuthorityRevision = f.Material.Policy.Revision()
	profile, _ := json.Marshal(in.Runtime.Profile)
	f.command.ProfileSHA256 = workspacebundle.Digest(profile)
	snapshot := f.Snapshot(1, true)
	snapshot.BaseSHA = in.SourceSHA
	snapshot.Issue.Title = "Candidate runtime conformance"
	snapshot.Issue.Body = "Synthetic fixture.\n## Acceptance criteria\n- Create AI_SDLC_EXECUTOR_FIXTURE.txt with the requested fixture text.\n- Preserve all original files.\n"
	if in.Scenario == "verification" {
		snapshot.Issue.Body = "Synthetic fixture.\n" + verification.RecipeMarker + "\n## Acceptance criteria\n- Correct locale fallback and add regression tests.\n"
	}
	snapshot.Revision = snapshot.ContentRevision()
	f.seedReadinessSnapshot(t, snapshot)
	if _, err = f.accept(f.envelope(t, f.command)); err != nil {
		t.Fatal("signed inbox", err)
	}
	root := t.TempDir()
	component := &bootstrap.Component{Name: "worker", DB: f.db, PolicyPath: filepath.Join(root, "policy"), IdentityPath: filepath.Join(root, "identity")}
	write := func(path string, v []byte) {
		t.Helper()
		if e := os.WriteFile(path, v, 0600); e != nil {
			t.Fatal("private fixture write")
		}
	}
	raw, _ = json.Marshal(f.Material.Policy)
	write(component.PolicyPath, raw)
	raw, _ = json.Marshal(f.Material.Identities["worker"])
	write(component.IdentityPath, raw)
	raw, _ = json.Marshal(in.Runtime)
	write(filepath.Join(root, "runtime"), raw)
	raw, _ = json.Marshal(f.Config)
	write(filepath.Join(root, "intake"), raw)
	write(filepath.Join(root, "token"), []byte(in.Config.BearerToken))
	write(filepath.Join(root, "ca"), in.Config.CA)
	write(filepath.Join(root, "model"), []byte(strings.Repeat("x", 48)))
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 48) {
			w.WriteHeader(401)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/readyz" {
			w.Write([]byte(`{"ready":true}`))
			return
		}
		n := calls.Add(1)
		if in.Scenario == "crash-after-model-start" {
			os.Exit(91)
		}
		message := map[string]any{"role": "assistant", "content": "Fixture file created."}
		reason := "stop"
		if in.Scenario == "verification" && n <= 2 {
			name := verification.LocaleSource
			if n == 2 {
				name = verification.LocaleNewTest
			}
			content, ok := in.Changes[name]
			if !ok {
				w.WriteHeader(500)
				return
			}
			args, _ := json.Marshal(map[string]string{"path": name, "content": content})
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "runtime-fixture", "type": "function", "function": map[string]any{"name": "write_file", "arguments": string(args)}}}}
			reason = "tool_calls"
		} else if n == 1 {
			args, _ := json.Marshal(map[string]string{"path": "AI_SDLC_EXECUTOR_FIXTURE.txt", "content": "Created by the isolated SCC executor conformance fixture.\n"})
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "runtime-fixture", "type": "function", "function": map[string]any{"name": "write_file", "arguments": string(args)}}}}
			reason = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "runtime-fixture", "object": "chat.completion", "model": "codex-cli", "choices": []any{map[string]any{"index": 0, "finish_reason": reason, "message": message}}})
	}))
	defer server.Close()
	runtime := Runtime{Component: component, ConfigPath: filepath.Join(root, "runtime"), IntakePath: filepath.Join(root, "intake"), TokenPath: filepath.Join(root, "token"), CAPath: filepath.Join(root, "ca"), GatewayTokenPath: filepath.Join(root, "model"), APIServer: in.Config.APIServer, ProviderEndpoint: server.URL + "/v1/chat/completions"}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	// CI's read-only identity stays in the trusted test process. It is never
	// serialized into fixture controls, Pod specs, model prompts or reports.
	if token := os.Getenv("AI_SDLC_TEST_SOURCE_TOKEN"); token != "" {
		scoped, err := sourcefetch.WithReadCredential(ctx, "os-santiago/homedir", token)
		if err != nil {
			t.Fatal("invalid fixture source credential")
		}
		ctx = scoped
	}

	state, err := runtime.PollOnce(ctx)
	if err != nil || state != "candidate-captured" {
		t.Fatal("runtime reconciliation", state, err)
	}
	if in.Scenario == "verification" {
		if state, e := runtime.PollOnce(ctx); e != nil || state != "verification-dispatched" {
			t.Fatal("operational verification handoff", state, e)
		}
		verifyCapturedFixture(t, f, component, in.VerificationDriver, in.VerificationConfig, in.Runtime.Placement.WorkerImage, in.ReportFile, f.command.ProfileSHA256, in.SourceSHA, in.VerificationScenario, calls.Load())
		return
	}
	if calls.Load() != 2 || f.Count(t, `SELECT count(*) FROM execution_candidates WHERE changes=1`) != 1 {
		t.Fatal("candidate or model handoff")
	}
	if state, err = runtime.PollOnce(ctx); err != nil || state != "" || calls.Load() != 2 {
		t.Fatal("runtime replayed claimed work", state, err)
	}
	var delta string
	var deltaRaw []byte
	if err = f.Admin.Pool.QueryRow(`SELECT delta FROM execution_candidates`).Scan(&deltaRaw); err != nil {
		t.Fatal(err)
	}
	decoded, err := candidate.Decode(deltaRaw)
	if err != nil || len(decoded.Changes) != 1 || decoded.Changes[0].Path != "AI_SDLC_EXECUTOR_FIXTURE.txt" || decoded.Changes[0].Before != nil || decoded.Changes[0].After == nil || string(decoded.Changes[0].After.Data) != "Created by the isolated SCC executor conformance fixture.\n" {
		t.Fatal("candidate differs from prescribed independent result")
	}

	if err = f.Admin.Pool.QueryRow(`SELECT delta_sha256 FROM execution_candidates`).Scan(&delta); err != nil {
		t.Fatal(err)
	}
	report, _ := json.Marshal(map[string]any{"scope": "isolated-candidate-runtime-fixture", "controller": "Runtime.PollOnce", "syntheticIssue": true, "syntheticModel": true, "realKubernetes": true, "realPostgres": true, "realSCC": true, "publicPinnedSourceFetched": true, "sourceSHA": in.SourceSHA, "candidateCaptured": true, "candidateDeltaSHA256": delta, "modelCalls": calls.Load(), "secondPollDidNotReplay": true, "autonomousE2E": false})
	write(in.ReportFile, report)
}

// A fresh process opens the original durable ledger after the preceding process
// exited abruptly during the provider call. It waits for real immutable expiry.
func TestRealCandidateRecoveryFixture(t *testing.T) {
	path := os.Getenv("AI_SDLC_CANDIDATE_FIXTURE")
	if path == "" {
		t.Skip("explicit disposable fixture required")
	}
	var in struct {
		Config                          podtransport.Config
		Runtime                         RuntimeConfig
		SourceSHA, ReportFile, Scenario string
	}
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &in) != nil || in.Scenario != "crash-after-model-start" {
		t.Fatal("fixture control")
	}
	u, err := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if err != nil || u.Path != "/ai_sdlc_test" {
		t.Fatal("disposable database required")
	}
	u.User = url.UserPassword("aisdlc_worker", "worker"+strings.Repeat("q", 40))
	db, err := store.Open(context.Background(), u.String())
	if err != nil {
		t.Fatal("worker identity")
	}
	defer db.Close()
	root := t.TempDir()
	write := func(name string, v []byte) {
		if os.WriteFile(filepath.Join(root, name), v, 0600) != nil {
			t.Fatal("private file")
		}
	}
	raw, _ = json.Marshal(in.Runtime)
	write("runtime", raw)
	write("token", []byte(in.Config.BearerToken))
	write("ca", in.Config.CA)
	runtime := Runtime{Component: &bootstrap.Component{Name: "worker", DB: db, PolicyPath: filepath.Join(root, "no-new-work-authority")}, ConfigPath: filepath.Join(root, "runtime"), TokenPath: filepath.Join(root, "token"), CAPath: filepath.Join(root, "ca"), APIServer: in.Config.APIServer}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	var initial int
	if db.Pool.QueryRow(`SELECT count(*) FROM execution_requests WHERE state='claimed'`).Scan(&initial) != nil || initial != 1 {
		t.Fatal("crash did not leave a durable owner")
	}
	for {
		_, _ = runtime.PollOnce(ctx) // New work remains denied; cleanup needs no model.
		var done int
		if db.Pool.QueryRow(`SELECT count(*) FROM execution_lifecycles WHERE cleanup_state='complete'`).Scan(&done) != nil {
			t.Fatal("cleanup observation")
		}
		if done == 1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("recovery deadline")
		case <-time.After(time.Second):
		}
	}
	for _, q := range []string{`SELECT count(*) FROM execution_requests WHERE state='stopped'`, `SELECT count(*) FROM model_budgets WHERE state='stopped' AND reserved_calls=1`, `SELECT count(*) FROM model_calls WHERE state='ambiguous'`, `SELECT count(*) FROM work_items WHERE status='failed'`} {
		var n int
		if db.Pool.QueryRow(q).Scan(&n) != nil || n != 1 {
			t.Fatal("recovery failed terminal invariant")
		}
	}
	var n int
	if db.Pool.QueryRow(`SELECT count(*) FROM execution_candidates`).Scan(&n) != nil || n != 0 {
		t.Fatal("crashed execution published candidate")
	}
	report, _ := json.Marshal(map[string]any{"scope": "isolated-candidate-recovery-fixture", "abruptProcessExit": true, "freshProcessRecovery": true, "realClaimExpiry": true, "realKubernetes": true, "realPostgres": true, "providerCallAmbiguous": true, "budgetClosedWithoutRefund": true, "workFailed": true, "cleanupByPersistedUID": true, "candidateCaptured": false, "autonomousE2E": false})
	if os.WriteFile(in.ReportFile, report, 0600) != nil {
		t.Fatal("report")
	}
}
