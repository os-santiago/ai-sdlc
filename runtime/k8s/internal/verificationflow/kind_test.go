package verificationflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func TestRealVerificationFlowFixture(t *testing.T) {
	path := os.Getenv("AI_SDLC_VERIFICATION_FLOW_FIXTURE")
	if path == "" {
		t.Skip("explicit disposable verification fixture required")
	}
	var in struct {
		Policy                                          authority.Policy
		Intake                                          intake.Config
		Profile, Image, ReportFile, SourceSHA, Scenario string
		Config                                          podtransport.Config
		Envelope, RetryEnvelope                         delivery.Envelope
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 256*1024 || json.Unmarshal(raw, &in) != nil {
		t.Fatal("private fixture input")
	}
	u, err := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if err != nil || u.Path != "/ai_sdlc_test" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		t.Fatal("disposable database required")
	}
	u.User = url.UserPassword("aisdlc_verifier", "verifier"+strings.Repeat("q", 40))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Second)
	defer cancel()
	db, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal("verifier identity")
	}
	defer db.Close()
	inbox := delivery.Inbox{DB: db, Target: "verifier"}
	for n, envelope := range []delivery.Envelope{in.Envelope, in.RetryEnvelope} {
		_, duplicate, e := inbox.Accept(ctx, in.Policy, envelope, "verify-artifact", http.MethodPost, "/internal/verify-artifact", time.Now(), Effect(func() (authority.Policy, error) { return in.Policy, nil }))
		if e != nil || duplicate != (n == 1) {
			t.Fatal("signed inbox/replay", e)
		}
	}
	load := func() (authority.Policy, intake.Config, string, error) { return in.Policy, in.Intake, in.Profile, nil }
	id, err := db.NextVerification(ctx)
	if err != nil || id == "" {
		t.Fatal("durable queue")
	}
	if _, _, _, err = CurrentProof(ctx, db, load, id); err != nil {
		t.Fatal("current proof", err)
	}
	revoked := func() (authority.Policy, intake.Config, string, error) {
		p := in.Policy
		p.Freeze = true
		return p, in.Intake, in.Profile, nil
	}
	if _, _, _, err = CurrentProof(ctx, db, revoked, id); err == nil {
		t.Fatal("revoked authority accepted")
	}
	var creates, deletes *atomic.Int32
	if in.Scenario == "lost-config-reply" {
		in.Config, creates, deletes = lostConfigReply(t, in.Config)
	}
	client, err := podtransport.NewClient(in.Config)
	if err != nil {
		t.Fatal("Kubernetes client")
	}
	if in.Scenario == "revoke-config" || in.Scenario == "revoke-results" {
		load = func() (authority.Policy, intake.Config, string, error) {
			var n int
			if err := db.Pool.QueryRow(`SELECT count(*) FROM verification_effects WHERE request_id=$1 AND resource_uid IS NOT NULL`, id).Scan(&n); err != nil {
				return authority.Policy{}, intake.Config{}, "", err
			}
			p := in.Policy
			threshold := 1
			if in.Scenario == "revoke-results" {
				threshold = 5
			}
			if n >= threshold {
				p.Freeze = true
			}
			return p, in.Intake, in.Profile, nil
		}
	}
	if in.Scenario == "expire-config" {
		expired := false
		load = func() (authority.Policy, intake.Config, string, error) {
			var n int
			if err := db.Pool.QueryRow(`SELECT count(*) FROM verification_effects WHERE request_id=$1 AND resource_uid IS NOT NULL`, id).Scan(&n); err != nil {
				return authority.Policy{}, intake.Config{}, "", err
			}
			if n >= 1 && !expired {
				// Fault injection in this disposable fixture only. The controller never
				// modifies or renews claim_until after its original claim transaction.
				if _, err := db.Pool.Exec(`UPDATE verification_requests SET claim_until=clock_timestamp()-interval '1 second' WHERE id=$1`, id); err != nil {
					return authority.Policy{}, intake.Config{}, "", err
				}
				expired = true
			}
			return in.Policy, in.Intake, in.Profile, nil
		}
	}
	runtime := Runtime{DB: db, Load: load, Client: client, Image: in.Image}
	state, err := runtime.PollOnce(ctx)
	if in.Scenario == "expire-config" {
		if err == nil || state != "stopped" {
			t.Fatal("expired claim continued", state, err)
		}
		if state, e := runtime.PollOnce(ctx); e != nil || state != "" {
			t.Fatal("expired recovery", state, e)
		}
		var stopped bool
		if db.Pool.QueryRow(`SELECT state='failed' AND proof IS NULL AND cleanup_state='complete' FROM verification_requests WHERE id=$1`, id).Scan(&stopped) != nil || !stopped {
			t.Fatal("expired claim not retired")
		}
		report, _ := json.Marshal(map[string]any{"scope": "isolated-candidate-verifier-expiry-fixture", "scenario": in.Scenario, "realKubernetes": true, "realPostgres": true, "realSCC": true, "syntheticIssue": true, "syntheticModel": true, "claimExpiryInjectedInDisposableDatabase": true, "noProofCommitted": true, "resourcesRetired": true, "secondPollDidNotReplay": true, "autonomousE2E": false})
		if os.WriteFile(in.ReportFile, report, 0600) != nil {
			t.Fatal("report")
		}
		return
	}

	if in.Scenario == "lost-config-reply" {
		if err == nil || state != "stopped" {
			t.Fatal("uncertain create continued", state, err)
		}
		_, _ = runtime.PollOnce(ctx)
		var uncertain bool
		if db.Pool.QueryRow(`SELECT state='failed' AND proof IS NULL AND cleanup_state='uncertain' FROM verification_requests WHERE id=$1`, id).Scan(&uncertain) != nil || !uncertain {
			t.Fatal("lost create not quarantined")
		}
		if creates.Load() != 1 || deletes.Load() != 0 {
			t.Fatal("unknown UID retried or deleted")
		}
		if state, e := runtime.PollOnce(ctx); e == nil || state != "cleanup-uncertain" {
			t.Fatal("uncertainty did not block", state, e)
		}
		report, _ := json.Marshal(map[string]any{"scope": "isolated-candidate-verifier-lost-reply-fixture", "scenario": in.Scenario, "realKubernetes": true, "realPostgres": true, "realSCC": true, "syntheticIssue": true, "syntheticModel": true, "createSucceededButUIDReplyLost": true, "noProofCommitted": true, "unknownResourcePreserved": true, "nextClaimBlocked": true, "createCalls": creates.Load(), "deleteCalls": deletes.Load(), "autonomousE2E": false})
		if os.WriteFile(in.ReportFile, report, 0600) != nil {
			t.Fatal("report")
		}
		return
	}

	if in.Scenario == "revoke-config" || in.Scenario == "revoke-results" {
		if err == nil || state != "stopped" {
			t.Fatal("revocation continued", state, err)
		}
		var stopped bool
		if db.Pool.QueryRow(`SELECT state='failed' AND proof IS NULL AND cleanup_state='complete' FROM verification_requests WHERE id=$1`, id).Scan(&stopped) != nil || !stopped {
			t.Fatal("revocation did not clean and fail closed")
		}
		if state, e := runtime.PollOnce(ctx); e != nil || state != "" {
			t.Fatal("revoked request replayed", state, e)
		}
		report, _ := json.Marshal(map[string]any{"scope": "isolated-candidate-verifier-revocation-fixture", "scenario": in.Scenario, "realKubernetes": true, "realPostgres": true, "realSCC": true, "syntheticIssue": true, "syntheticModel": true, "authorityRevokedDuringReconciliation": true, "noProofCommitted": true, "resourcesRetired": true, "secondPollDidNotReplay": true, "autonomousE2E": false})
		if os.WriteFile(in.ReportFile, report, 0600) != nil {
			t.Fatal("report")
		}
		return
	}
	if err != nil || state != "behavior-verified" {
		t.Fatal("verification runtime", state, err)
	}
	var proofRaw []byte
	if db.Pool.QueryRow(`SELECT proof FROM verification_requests WHERE id=$1 AND state='passed' AND cleanup_state='complete'`, id).Scan(&proofRaw) != nil {
		t.Fatal("durable proof")
	}
	var proof BehaviorProof
	if json.Unmarshal(proofRaw, &proof) != nil || !proof.BehaviorVerified || proof.PublicationAuthorized || len(proof.ResourceUIDs) != 5 {
		t.Fatal("proof scope")
	}
	if state, err = runtime.PollOnce(ctx); err != nil || state != "" {
		t.Fatal("verification replay", state, err)
	}
	report, _ := json.Marshal(map[string]any{"scope": "isolated-candidate-verifier-runtime-fixture", "syntheticIssue": true, "syntheticModel": true, "realSCC": true, "realKubernetes": true, "realPostgres": true, "signedWorkerOutbox": true, "signedVerifierInbox": true, "duplicateInboxDidNotReplay": true, "currentAuthorityChecked": true, "separateOfflinePods": 4, "sourceSHA": in.SourceSHA, "behaviorProof": proof, "secondPollDidNotReplay": true, "autonomousE2E": false})
	if os.WriteFile(in.ReportFile, report, 0600) != nil {
		t.Fatal("report")
	}
}
