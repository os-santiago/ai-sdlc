package reviewflow

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/sourcefetch"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
)

// Same private subprocess contract as the behavior fixture, extended through
// real readonly SCC, Kubernetes helpers and independent model journal. All
// identities, grants, HTTP replies and issues here belong to disposable tests.
func TestRealVerificationFlowFixture(t *testing.T) {
	path := os.Getenv("AI_SDLC_VERIFICATION_FLOW_FIXTURE")
	reviewPath := os.Getenv("AI_SDLC_REVIEW_CONTROL")
	if path == "" || reviewPath == "" {
		t.Skip("explicit isolated review controller fixture required")
	}
	var in struct {
		Policy                                          authority.Policy
		Identity                                        authority.Identity
		Intake                                          intake.Config
		Profile, Image, ReportFile, SourceSHA, Scenario string
		Config                                          podtransport.Config
		Envelope, RetryEnvelope                         delivery.Envelope
	}
	raw, e := os.ReadFile(path)
	if e != nil || len(raw) > 262144 || json.Unmarshal(raw, &in) != nil {
		t.Fatal("verifier fixture input")
	}
	var control struct {
		Config                 podtransport.Config
		MailboxImage, Scenario string
		Endpoint, Token        string
		ADev                   []byte
	}
	raw, e = os.ReadFile(reviewPath)
	if e != nil || len(raw) > 262144 || json.Unmarshal(raw, &control) != nil || control.Config.APIServer != in.Config.APIServer {
		t.Fatal("review fixture input")
	}
	if control.Scenario != "approve" && control.Scenario != "request_changes" && control.Scenario != "mutation" && control.Scenario != "revoke-model" && control.Scenario != "real-model" && control.Scenario != "lost-config-reply" && control.Scenario != "revoke-config" {
		t.Fatal("review fixture scenario")
	}
	u, e := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if e != nil || u.Path != "/ai_sdlc_test" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		t.Fatal("disposable database required")
	}
	if control.Scenario == "real-model" {
		endpoint, e := url.Parse(control.Endpoint)
		if e != nil || endpoint.Scheme != "http" || endpoint.Hostname() != "127.0.0.1" || endpoint.Path != "/v1/chat/completions" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			t.Fatal("owned loopback provider required")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Second)
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

	admin, e := store.Open(ctx, u.String())
	if e != nil {
		t.Fatal("fixture database")
	}
	defer admin.Close()
	if _, e = admin.Pool.Exec(`GRANT SELECT ON execution_requests TO aisdlc_verifier; GRANT SELECT,INSERT,UPDATE ON review_runs,review_effects,review_model_calls TO aisdlc_verifier`); e != nil {
		t.Fatal("disposable review grants")
	}
	u.User = url.UserPassword("aisdlc_verifier", "verifier"+strings.Repeat("q", 40))
	db, e := store.Open(ctx, u.String())
	if e != nil {
		t.Fatal("verifier identity")
	}
	defer db.Close()
	inbox := delivery.Inbox{DB: db, Target: "verifier"}
	t.Setenv("AI_SDLC_RUNTIME_MODE", "verification")
	mux := http.NewServeMux()
	mux.Handle("POST /internal/verify-artifact", inbox.Handler(func() (authority.Policy, authority.Identity, error) { return in.Policy, in.Identity, nil }, "verify-artifact", "/internal/verify-artifact", verificationflow.Effect(func() (authority.Policy, error) { return in.Policy, nil })))
	server := httptest.NewServer(service.RuntimeBoundary(mux))
	defer server.Close()
	for _, envelope := range []delivery.Envelope{in.Envelope, in.RetryEnvelope} {
		raw, _ := json.Marshal(envelope)
		response, e := http.Post(server.URL+"/internal/verify-artifact", "application/json", bytes.NewReader(raw))
		if e != nil {
			t.Fatal("signed inbox")
		}
		io.Copy(io.Discard, io.LimitReader(response.Body, 65536))
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal("signed inbox", response.StatusCode)
		}
	}
	var revoked atomic.Bool
	policy := func() (authority.Policy, intake.Config, string, error) {
		p := in.Policy
		if control.Scenario == "revoke-config" {
			var count int
			if db.Pool.QueryRow(`SELECT count(*) FROM review_effects WHERE kind='config' AND completed_at IS NOT NULL`).Scan(&count) != nil {
				return authority.Policy{}, intake.Config{}, "", ErrReview
			}
			if count > 0 {
				p.Freeze = true
			}
		}
		if revoked.Load() {
			p.Freeze = true
		}
		return p, in.Intake, in.Profile, nil
	}
	client, e := podtransport.NewClient(in.Config)
	if e != nil {
		t.Fatal("behavior Kubernetes")
	}
	verifier := verificationflow.Runtime{DB: db, Load: policy, Client: client, Image: in.Image}
	if state, e := verifier.PollOnce(ctx); e != nil || state != "behavior-verified" {
		t.Fatal("behavior prerequisite", state, e)
	}
	id, e := db.NextVerifiedForReview(ctx)
	if e != nil || id == "" {
		t.Fatal("verified review input")
	}
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, e := io.ReadAll(io.LimitReader(r.Body, 131073))
		if e != nil || len(raw) > 131072 {
			w.WriteHeader(400)
			return
		}
		n := calls.Add(1)
		if control.Scenario == "real-model" {
			req, e := http.NewRequestWithContext(r.Context(), http.MethodPost, control.Endpoint, bytes.NewReader(raw))
			if e != nil {
				w.WriteHeader(502)
				return
			}
			req.Header.Set("Authorization", "Bearer "+control.Token)
			req.Header.Set("Content-Type", "application/json")
			client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			response, e := client.Do(req)
			if e != nil {
				w.WriteHeader(502)
				return
			}
			defer response.Body.Close()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(response.StatusCode)
			body, _ := io.ReadAll(io.LimitReader(response.Body, 32769))
			var diagnostic struct {
				Model   string `json:"model"`
				Choices []struct {
					Finish  string `json:"finish_reason"`
					Message struct {
						Content *string `json:"content"`
						Tools   []struct {
							Function struct {
								Name string `json:"name"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"message"`
				} `json:"choices"`
			}
			if json.Unmarshal(body, &diagnostic) == nil && len(diagnostic.Choices) == 1 {
				choice := diagnostic.Choices[0]
				names := []string{}
				for _, tool := range choice.Message.Tools {
					n := tool.Function.Name
					switch n {
					case "read_file", "list_dir", "search_text", "code_query":
					default:
						n = "unsupported"
					}
					names = append(names, n)
				}
				size := 0
				if choice.Message.Content != nil {
					size = len(*choice.Message.Content)
				}
				t.Logf("owned provider observation: status=%d expectedModel=%v contentBytes=%d tools=%v", response.StatusCode, diagnostic.Model == "codex-cli", size, names)
			}
			w.Write(body)
			return
		}
		finish := "stop"
		verdict := control.Scenario
		if verdict != "request_changes" {
			verdict = "approve"
		}
		message := map[string]any{"role": "assistant", "content": "Inspected the verified locale candidate.\nVERDICT: " + verdict}
		if n == 1 {
			name, args := "read_file", `{"path":"quarkus-app/src/main/resources/META-INF/resources/js/utils.js"}`
			if control.Scenario == "mutation" {
				name, args = "run_shell", `{"command":"true"}`
			}
			finish = "tool_calls"
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"id": "inspect-1", "type": "function", "function": map[string]string{"name": name, "arguments": args}}}
		}
		if control.Scenario == "revoke-model" {
			revoked.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"model": "review-model", "choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}}})
	}))
	defer provider.Close()
	model, steps, seconds := "review-model", 4, 90
	if control.Scenario == "real-model" {
		model, steps, seconds = "codex-cli", 8, 180
	}
	profile, _ := json.Marshal(map[string]any{"version": "ai-sdlc.reviewer-profile/v1", "model": model, "maxSteps": steps, "maxSeconds": seconds, "maxTokens": 2048, "aDevSHA256": authority.Digest(control.ADev)})
	config := Configuration{Namespace: "ai-sdlc-reviewers", WorkerImage: in.Image, MailboxImage: control.MailboxImage, Profile: profile, ADev: control.ADev, Model: modelbroker.ProviderConfig{Endpoint: provider.URL + "/v1/chat/completions", BearerToken: strings.Repeat("x", 48), AllowLoopbackHTTP: true}}
	load := func(context.Context) (Configuration, error) { return config, nil }
	var creates, deletes *atomic.Int32
	if control.Scenario == "lost-config-reply" {
		control.Config, creates, deletes = lostConfigReply(t, control.Config)
	}
	reviewClient, e := podtransport.NewClient(control.Config)
	if e != nil {
		t.Fatal("review Kubernetes")
	}
	runtime := Runtime{DB: db, Policy: policy, Load: load, Client: reviewClient}
	state, runErr := runtime.PollOnce(ctx)
	var storedState, cleanup, reason string
	var proofRaw []byte
	var handed int
	if db.Pool.QueryRow(`SELECT state,cleanup_state,reason,proof FROM review_runs WHERE verification_id=$1`, id).Scan(&storedState, &cleanup, &reason, &proofRaw) != nil {
		t.Fatal("review journal")
	}
	if db.Pool.QueryRow(`SELECT count(*) FROM review_model_calls WHERE state='handed-off'`).Scan(&handed) != nil {
		t.Fatal("handoff evidence")
	}
	if control.Scenario == "lost-config-reply" {
		if runErr == nil || storedState != "failed" || cleanup != "uncertain" || len(proofRaw) != 0 || calls.Load() != 0 || creates.Load() != 1 || deletes.Load() != 0 {
			t.Fatal("unknown create not quarantined", storedState, cleanup, runErr)
		}
	} else if control.Scenario == "approve" || (control.Scenario == "real-model" && storedState == "passed") {
		var proof Proof
		if runErr != nil || state != "review-passed" || storedState != "passed" || cleanup != "complete" || (calls.Load() < 2 || calls.Load() > 8 || handed != int(calls.Load())) || json.Unmarshal(proofRaw, &proof) != nil || proof.Verdict != "approve" || !proof.SourceUnchanged || proof.PublicationAuthorized {
			t.Fatal("review proof", state, storedState, cleanup, calls.Load(), handed, runErr)
		}
	} else {
		if runErr == nil || storedState != "failed" || cleanup != "complete" || len(proofRaw) != 0 {
			t.Fatal("unsafe review accepted", state, storedState, cleanup, runErr)
		}
		if (control.Scenario == "request_changes" || control.Scenario == "real-model") && (reason != "request-changes" || handed < 2 || handed != int(calls.Load())) {
			t.Fatal("request changes result", reason, handed, calls.Load(), runErr)
		}
		expectedCalls := int32(1)
		if control.Scenario == "revoke-config" {
			expectedCalls = 0
		}
		if control.Scenario != "request_changes" && control.Scenario != "real-model" && (handed != 0 || calls.Load() != expectedCalls) {
			t.Fatal("unsafe response reached SCC")
		}
	}
	before := calls.Load()
	next, pollErr := runtime.PollOnce(ctx)
	if control.Scenario == "lost-config-reply" {
		if pollErr == nil || next != "cleanup-uncertain" || calls.Load() != before || creates.Load() != 1 || deletes.Load() != 0 {
			t.Fatal("unknown create replayed")
		}
	} else if pollErr != nil || calls.Load() != before {
		t.Fatal("review replay", pollErr)
	}
	report, _ := json.Marshal(map[string]any{"scope": "isolated-candidate-behavior-review-controller", "scenario": control.Scenario, "realSCC": true, "realKubernetes": true, "realPostgres": true, "realHTTP": true, "syntheticIssue": true, "syntheticReviewModel": control.Scenario != "real-model", "syntheticGenerationModel": true, "realCodexCLI": control.Scenario == "real-model", "signedCandidateHandoff": true, "behaviorVerified": true, "reviewState": storedState, "cleanupState": cleanup, "modelCalls": calls.Load(), "confirmedHandoffs": handed, "proofCommitted": len(proofRaw) > 0, "secondPollDidNotReplay": true, "publicationAuthorized": false, "autonomousE2E": false})
	if os.WriteFile(in.ReportFile, report, 0600) != nil {
		t.Fatal("report")
	}
}
