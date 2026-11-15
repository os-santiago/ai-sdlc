// Package testsupport creates only explicitly selected disposable database
// fixtures. Production components never import this package.
package testsupport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

type Fixture struct {
	Admin      *store.DB
	Material   authority.BootstrapMaterial
	Config     intake.Config
	Components map[string]*bootstrap.Component
}

func New(t *testing.T) Fixture {
	t.Helper()
	connection := os.Getenv("AI_SDLC_TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("disposable PostgreSQL required")
	}
	ctx := context.Background()
	admin, err := store.Open(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	if err := admin.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Pool.ExecContext(ctx, `TRUNCATE authorized_outbox,authorized_inbox,authorized_nonces,admissions,admission_heads,planning_heads,plans,work_items,attempts CASCADE`); err != nil {
		t.Fatal(err)
	}
	material, err := authority.Bootstrap("example/project", "production-test")
	if err != nil {
		t.Fatal(err)
	}
	material.Policy.Freeze = false
	passwords := map[string]string{}
	for role := range material.Identities {
		passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
	}
	if err := admin.ProvisionRoles(ctx, passwords); err != nil {
		t.Fatal(err)
	}
	f := Fixture{Admin: admin, Material: material, Config: intake.Config{Version: 1, Repository: "example/project", BaseBranch: "main", TriggerLabel: "ready", AuthorizedActorIDs: []int64{42}, PollSeconds: 30, MaxPages: 3}, Components: map[string]*bootstrap.Component{}}
	dir := t.TempDir()
	policy, _ := json.Marshal(material.Policy)
	policyPath := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(policyPath, policy, 0600); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"admission-controller", "orchestrator"} {
		u, err := url.Parse(connection)
		if err != nil {
			t.Fatal(err)
		}
		name, _ := store.ComponentRole(role)
		u.User = url.UserPassword(name, passwords[role])
		db, err := store.Open(ctx, u.String())
		if err != nil {
			t.Fatal("fixture role connection failed")
		}
		t.Cleanup(func() { db.Close() })
		identity, _ := json.Marshal(material.Identities[role])
		identityPath := filepath.Join(dir, role+".json")
		if err := os.WriteFile(identityPath, identity, 0600); err != nil {
			t.Fatal(err)
		}
		f.Components[role] = &bootstrap.Component{Name: role, DB: db, PolicyPath: policyPath, IdentityPath: identityPath}
	}
	return f
}
func (f Fixture) Snapshot(generation int64, eligible bool) intake.Snapshot {
	issue := forge.Issue{ID: 7, Number: 3, Title: "docs: clarify installation", Body: strings.Repeat("Document the supported local setup and its observable behavior. ", 3) + "\n## Acceptance criteria\n- The local setup is documented.\n", State: "open", User: forge.Actor{ID: 42, Login: "fixture"}, Labels: []forge.Label{{Name: "ready"}}, UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if !eligible {
		issue.State = "closed"
	}
	s := intake.Snapshot{Version: 1, Repository: f.Config.Repository, Issue: issue, Generation: generation, AuthorityRevision: f.Material.Policy.Revision(), ConfigDigest: f.Config.Digest(), BaseSHA: strings.Repeat("a", 40), AuthorizedActor: forge.Actor{ID: 42, Login: "fixture"}, LabelEventID: 9, Eligible: eligible}
	s.Revision = s.ContentRevision()
	return s
}
func (f Fixture) Envelope(t *testing.T, producer, id, operation, path string, value any) delivery.Envelope {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := f.Material.Identities[producer].Seal(f.Material.Policy, id, operation, raw)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := f.Material.Identities["relay"].Carry(f.Material.Policy, receipt, http.MethodPost, path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return delivery.Envelope{Receipt: receipt, Transport: transport, Payload: raw}
}
func (f Fixture) Count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := f.Admin.Pool.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
