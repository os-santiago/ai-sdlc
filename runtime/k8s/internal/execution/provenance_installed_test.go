package execution

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenanceflow"
)

func provenanceInstallation(p provenanceFixture) buildflow.Installed {
	return buildflow.Installed{Version: "ai-sdlc.build-installation/v1", Inner: p.cfg.Build.Inner, Outer: p.cfg.Build.Outer, Profile: p.record.Context.Profile, Slots: p.cfg.Build.Slots, IntakeConfigSHA256: p.cfg.Build.IntakeConfigSHA256, IssueMaxAgeSeconds: p.cfg.Build.IssueMaxAgeSeconds}
}

func TestProvenanceInstalledMountsFailClosedOnCurrentConfiguration(t *testing.T) {
	p := newProvenanceFixture(t)
	dir := t.TempDir()
	m := provenanceflow.Mounts{Build: filepath.Join(dir, "build.json"), Inner: filepath.Join(dir, "inner.json"), Outer: filepath.Join(dir, "outer.json"), Trust: filepath.Join(dir, "trust.json"), Identity: filepath.Join(dir, "identity.json"), Credential: filepath.Join(dir, "credential.json"), VerifierImage: p.cfg.Trust.VerifierImage}
	write := func(path string, value any) {
		t.Helper()
		raw, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(path, raw, 0600); e != nil {
			t.Fatal(e)
		}
	}
	install := provenanceInstallation(p)
	restore := func() {
		write(m.Build, install)
		write(m.Inner, install.Inner)
		write(m.Outer, install.Outer)
		write(m.Trust, p.cfg.Trust)
		write(m.Identity, p.b.f.Material.Identities["verifier"])
		write(m.Credential, map[string]string{"username": "ai-sdlc-verifier", "password": strings.Repeat("x", 40)})
	}
	restore()
	cfg, key, e := m.Load("verifier")
	if e != nil || key.Role != "verifier" || cfg.Trust.Digest() != p.cfg.Trust.Digest() {
		t.Fatal("installed verifier", e)
	}
	if _, e = m.Verifier(cfg); e != nil {
		t.Fatal("registry factory", e)
	}
	// The dedicated process has no dependency on the inner model gateway or
	// candidate configuration. Its role/canonical mounts still gate readiness.
	t.Setenv("AI_SDLC_EXECUTION_CONFIG_PATH", filepath.Join(dir, "missing-execution.json"))
	dedicated := provenanceflow.Service{DB: p.j.DB, Mounts: m}
	if e := dedicated.Ready(context.Background()); e != nil {
		t.Fatal("independent readiness", e)
	}
	// Isolated SQL projections for unrelated inner activity: preserve the real
	// original rows and append a synthetic second review without foreign keys.
	// These rows test containment selection only, never provide stage authority.
	schema := "containment_" + strings.ReplaceAll(store.NewID(), "-", "")
	if _, e := p.b.f.Admin.Pool.Exec(`CREATE SCHEMA ` + schema); e != nil {
		t.Fatal(e)
	}
	defer p.b.f.Admin.Pool.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
	for _, table := range []string{"review_runs", "verification_requests"} {
		if _, e := p.b.f.Admin.Pool.Exec(`CREATE TABLE ` + schema + `.` + table + ` (LIKE public.` + table + ` INCLUDING ALL); INSERT INTO ` + schema + `.` + table + ` SELECT * FROM public.` + table + `; GRANT SELECT ON ` + schema + `.` + table + ` TO aisdlc_verifier`); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := p.b.f.Admin.Pool.Exec(`GRANT USAGE ON SCHEMA ` + schema + ` TO aisdlc_verifier`); e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	u.User = url.UserPassword("aisdlc_verifier", p.b.passwords["verifier"])
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	projected, e := store.Open(context.Background(), u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer projected.Close()
	dedicated.DB = projected
	patch := map[string]any{"id": "review-" + strings.Repeat("9", 64), "verification_id": "verification-" + strings.Repeat("9", 64), "candidate_id": "execution-" + strings.Repeat("9", 64), "state": "claimed", "cleanup_state": "pending", "proof": nil, "claim_token": store.NewID(), "claim_until": time.Now().Add(5 * time.Minute), "expires_at": time.Now().Add(6 * time.Minute), "reason": ""}
	change, _ := json.Marshal(patch)
	if _, e := p.b.f.Admin.Pool.Exec(`INSERT INTO `+schema+`.review_runs SELECT (jsonb_populate_record(NULL::`+schema+`.review_runs,to_jsonb(original)||$1::jsonb)).* FROM public.review_runs original LIMIT 1`, string(change)); e != nil {
		t.Fatal(e)
	}
	if e := dedicated.Ready(context.Background()); e != nil {
		t.Fatal("live slow inner review blocked dedicated process", e)
	}
	if _, e := p.b.f.Admin.Pool.Exec(`UPDATE `+schema+`.review_runs SET state='failed',reason='uncertain',cleanup_state='uncertain' WHERE id=$1`, patch["id"]); e != nil {
		t.Fatal(e)
	}
	if _, e := dedicated.Load(); e == nil {
		t.Fatal("uncertain review custody accepted")
	}
	if e := dedicated.Ready(context.Background()); e == nil {
		t.Fatal("uncertain containment marked ready")
	}
	cases := map[string]func(){
		"freeze-canonical-outer": func() { v := install.Outer; v.Freeze = true; write(m.Outer, v) },
		"freeze-canonical-inner": func() { v := install.Inner; v.Freeze = true; write(m.Inner, v) },
		"expired-trust":          func() { v := p.cfg.Trust; v.ExpiresAt = time.Now().Add(-time.Second).UnixMilli(); write(m.Trust, v) },
		"substituted-image": func() {
			v := p.cfg.Trust
			v.VerifierImage = "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("a", 64)
			write(m.Trust, v)
		},
		"substituted-publisher": func() {
			v := p.cfg.Trust
			v.PublisherImage = "localhost/other@sha256:" + strings.Repeat("a", 64)
			write(m.Trust, v)
		},
		"substituted-cluster": func() { v := p.cfg.Trust; v.ClusterUID = "99999999-9999-9999-9999-999999999999"; write(m.Trust, v) },
		"wrong-identity":      func() { write(m.Identity, p.b.f.Material.Identities["build-controller"]) },
		"missing-trust": func() {
			if e := os.Remove(m.Trust); e != nil {
				t.Fatal(e)
			}
		},
		"oversized-trust": func() {
			if e := os.WriteFile(m.Trust, []byte(strings.Repeat("x", 128*1024+1)), 0600); e != nil {
				t.Fatal(e)
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			restore()
			change()
			if _, _, e := m.Load("verifier"); e == nil {
				t.Fatal("invalid installation accepted")
			}
		})
	}
	restore()
	write(m.Identity, p.b.f.Material.Identities["build-controller"])
	if e := os.Remove(m.Credential); e != nil {
		t.Fatal(e)
	}
	if _, _, e := m.Load("build-controller"); e != nil {
		t.Fatal("dispatcher required registry custody", e)
	}
	if _, _, e := m.Load("worker"); e == nil {
		t.Fatal("wrong role accepted")
	}
	if _, e := m.Verifier(cfg); e == nil {
		t.Fatal("missing credential accepted")
	}
	restore()
	write(m.Credential, map[string]string{"username": "ai-sdlc-publisher", "password": strings.Repeat("x", 40)})
	if _, e := m.Verifier(cfg); e == nil {
		t.Fatal("publisher credential accepted")
	}
	// Canonical schema rejects aliases and duplicate keys as well as unknowns.
	raw, _ := json.Marshal(p.cfg.Trust)
	for _, bad := range [][]byte{[]byte(strings.Replace(string(raw), `"version":`, `"Version":`, 1)), append([]byte(`{"version":"wrong",`), raw[1:]...)} {
		if e := os.WriteFile(m.Trust, bad, 0600); e != nil {
			t.Fatal(e)
		}
		if _, _, e := m.Load("verifier"); e == nil {
			t.Fatal("noncanonical trust accepted")
		}
	}
}

func TestBuildProvenanceHandoffRemainsBehindOriginalContainment(t *testing.T) {
	p := newProvenanceFixture(t)
	ctx := context.Background()
	calls := 0
	// Real TLS transport refuses cleanup. This is an isolated API-failure fixture,
	// not a claim that original Kubernetes resources were actually deleted.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer server.Close()
	client, e := podtransport.NewClient(podtransport.Config{APIServer: server.URL, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), BearerToken: strings.Repeat("x", 32)})
	if e != nil {
		t.Fatal(e)
	}
	runtime := buildflow.Runtime{DB: p.b.journal.DB, Load: func() (buildflow.Installed, error) { return provenanceInstallation(p), nil }, Client: func() (*podtransport.Client, error) { return client, nil }, AfterRecovery: func(ctx context.Context) (bool, error) {
		calls++
		return provenanceflow.DispatchBuilt(ctx, p.b.journal.DB, func() (provenance.Configuration, authority.Identity, error) {
			return *p.cfg, p.b.f.Material.Identities["build-controller"], nil
		})
	}}
	state, e := runtime.PollOnce(ctx)
	if e == nil || state != "build-containment" || calls != 0 {
		t.Fatal("handoff bypassed failed original containment", state, e, calls)
	}
	provenanceCleanupFixture(t, p) // Explicitly synthetic journal acknowledgement.
	state, e = runtime.PollOnce(ctx)
	if e != nil || state != "build-verification-handoff" || calls != 1 {
		t.Fatal("cleaned build did not dispatch", state, e, calls)
	}
	if p.b.f.Count(t, `SELECT count(*) FROM provenance_handoffs`) != 1 {
		t.Fatal("handoff not journaled")
	}
	state, e = runtime.PollOnce(ctx)
	if e != nil || state != "build-idle" || calls != 2 || p.b.f.Count(t, `SELECT count(*) FROM provenance_handoffs`) != 1 {
		t.Fatal("duplicate handoff", state, e, calls)
	}
}
