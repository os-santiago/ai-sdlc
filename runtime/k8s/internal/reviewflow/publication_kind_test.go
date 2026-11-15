package reviewflow

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publication"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publicationflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// Explicitly synthetic forge, alongside the real signed HTTP inbox, PostgreSQL,
// source reconstruction and preceding real SCC/Kubernetes behavior/review flow.
type fixturePublisher struct {
	calls    int
	scenario string
	revoke   *bool
}

func (w *fixturePublisher) step(ctx context.Context, f publication.WriteFence, kind string) error {
	digest := authority.Digest([]byte(kind))
	if e := f(ctx, digest); e != nil {
		return e
	}
	w.calls++
	if w.scenario == "lost-"+kind {
		return publication.ErrPublication
	}
	if w.scenario == "revoked-tree" && kind == "tree" {
		*w.revoke = true
	}
	return f(ctx, digest)
}
func (w *fixturePublisher) CreateTree(ctx context.Context, f publication.WriteFence, p publication.TreePlan) (string, error) {
	return p.ExpectedTreeSHA, w.step(ctx, f, "tree")
}
func (w *fixturePublisher) CreateCommit(ctx context.Context, f publication.WriteFence, p publication.TreePlan, source string, date time.Time) (string, error) {
	return strings.Repeat("a", 40), w.step(ctx, f, "commit")
}
func (w *fixturePublisher) CreateBranch(ctx context.Context, f publication.WriteFence, p publication.TreePlan, sha string) error {
	return w.step(ctx, f, "branch")
}
func (w *fixturePublisher) CreatePullRequest(ctx context.Context, f publication.WriteFence, p publication.TreePlan, source, sha, digest string) (publication.PublishedPullRequest, error) {
	return publication.PublishedPullRequest{Number: 42, HeadSHA: sha, Branch: p.Branch, URL: "https://github.com/os-santiago/homedir/pull/42"}, w.step(ctx, f, "pull-request")
}

func verifyPublicationFixture(t *testing.T, admin, verifier *store.DB, original verificationflow.PolicyLoader, config Configuration, p authority.Policy, key authority.Identity, identities map[string]authority.Identity) {
	t.Helper()
	if identities["release-manager"].Validate(p) != nil || identities["relay"].Validate(p) != nil {
		t.Fatal("private publication fixture identities")
	}
	u, e := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if e != nil || u.Path != "/ai_sdlc_test" || u.Hostname() != "127.0.0.1" {
		t.Fatal("disposable database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	passwords := map[string]string{}
	for _, key := range p.Keys {
		passwords[key.Role] = strings.ReplaceAll(key.Role, "-", "_") + strings.Repeat("q", 40)
	}
	if e = admin.ProvisionRolesForCapabilities(ctx, passwords, "verification", true, true); e != nil {
		t.Fatal("installed publication capabilities", e)
	}
	u.User = url.UserPassword("aisdlc_release_manager", "release_manager"+strings.Repeat("q", 40))
	db, e := store.Open(ctx, u.String())
	if e != nil {
		t.Fatal("publisher fixture identity")
	}
	defer db.Close()
	review := func() (publicationflow.ReviewPolicy, error) {
		return publicationflow.ReviewPolicy{Profile: authority.Digest(config.Profile), Configuration: config.Digest()}, nil
	}
	identity := func() (authority.Policy, authority.Identity, error) { return p, key, nil }
	results := []map[string]any{}
	for _, scenario := range []string{"success", "lost-tree", "revoked-tree", "lost-pull-request"} {
		if _, e = admin.Pool.Exec(`DELETE FROM authorized_nonces WHERE target='release-manager'; DELETE FROM authorized_inbox WHERE target='release-manager'; DELETE FROM authorized_outbox WHERE operation='publish-candidate'; DELETE FROM published_records; DELETE FROM publication_effects; DELETE FROM publication_requests; DELETE FROM publication_handoffs`); e != nil {
			t.Fatal("reset owned publication fixture", e)
		}
		revoked := false
		policy := func() (authority.Policy, intake.Config, string, error) {
			p, c, profile, e := original()
			if revoked {
				p.Freeze = true
			}
			return p, c, profile, e
		}
		if sent, e := publicationflow.Handoff(ctx, verifier, policy, review, identity); e != nil || !sent {
			t.Fatal("publication handoff", scenario, e)
		}
		if sent, e := publicationflow.Handoff(ctx, verifier, policy, review, identity); e != nil || sent {
			t.Fatal("publication handoff replay", e)
		}
		var envelope delivery.Envelope
		var receipt []byte
		if admin.Pool.QueryRow(`SELECT payload,receipt FROM authorized_outbox WHERE operation='publish-candidate'`).Scan(&envelope.Payload, &receipt) != nil || json.Unmarshal(receipt, &envelope.Receipt) != nil {
			t.Fatal("publication outbox")
		}
		inbox := delivery.Inbox{DB: db, Target: "release-manager"}
		load := func() (authority.Policy, authority.Identity, error) { return p, identities["release-manager"], nil }
		server := httptest.NewServer(inbox.Handler(load, "publish-candidate", "/internal/publish-candidate", publicationflow.Effect(func() (authority.Policy, error) { return p, nil })))
		for range 2 {
			envelope.Transport, e = identities["relay"].Carry(p, envelope.Receipt, http.MethodPost, "/internal/publish-candidate", time.Now())
			if e != nil {
				t.Fatal("publication relay")
			}
			raw, _ := json.Marshal(envelope)
			response, e := http.Post(server.URL+"/internal/publish-candidate", "application/json", bytes.NewReader(raw))
			if e != nil {
				t.Fatal("publication HTTP inbox")
			}
			io.Copy(io.Discard, io.LimitReader(response.Body, 65536))
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatal("publication inbox", response.StatusCode)
			}
		}
		server.Close()
		publisher := &fixturePublisher{scenario: scenario, revoke: &revoked}
		runtime := publicationflow.Runtime{DB: db, Policy: policy, Review: review, Writer: publisher}
		state, runErr := runtime.PollOnce(ctx)
		var stored, publicationID string
		var proof []byte
		if db.Pool.QueryRow(`SELECT id,state,proof FROM publication_requests`).Scan(&publicationID, &stored, &proof) != nil {
			t.Fatal("publication evidence")
		}
		expectedCalls := 1
		if scenario == "success" || scenario == "lost-pull-request" {
			expectedCalls = 4
		}
		if publisher.calls != expectedCalls {
			t.Fatal("publication effects", scenario, publisher.calls, state, runErr)
		}
		if scenario == "success" {
			var p publicationflow.Proof
			if runErr != nil || state != "pull-request-published" || stored != "published" || json.Unmarshal(proof, &p) != nil || p.MergeAuthorized || p.DeploymentAuthorized || p.PullRequest != 42 {
				t.Fatal("publication proof", state, stored, runErr)
			}
		} else if runErr == nil || stored != "failed" || len(proof) != 0 {
			t.Fatal("ambiguous publication accepted", scenario, state, stored, runErr)
		}
		again, e := runtime.PollOnce(ctx)
		if e != nil || publisher.calls != expectedCalls {
			t.Fatal("publication replay", again, e)
		}
		if scenario != "success" && again != "publication-quarantined" {
			t.Fatal("publication not quarantined", again)
		}
		_, recordError := db.ReadPublishedRecord(ctx, publicationID)
		if (scenario == "success") != (recordError == nil) {
			t.Fatal("durable publication record differs from confirmed outcome", scenario, recordError)
		}
		results = append(results, map[string]any{"scenario": scenario, "state": stored, "effects": publisher.calls, "secondPoll": again, "noReplay": true, "durableRecord": recordError == nil})
	}
	if path := os.Getenv("AI_SDLC_PUBLICATION_REPORT"); path != "" {
		raw, _ := json.MarshalIndent(map[string]any{"scope": "isolated-publication-controller", "realPostgres": true, "realSignedHTTPInbox": true, "realCandidateBehaviorReviewPrerequisite": true, "syntheticForge": true, "installedColumnPrivileges": true, "githubWrites": false, "autonomousE2E": false, "scenarios": results}, "", "  ")
		if os.WriteFile(path, raw, 0600) != nil {
			t.Fatal("publication fixture report")
		}
	}
}
