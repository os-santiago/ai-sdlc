package bootstrap

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func files(t *testing.T) (*Component, authority.BootstrapMaterial) {
	t.Helper()
	material, err := authority.Bootstrap("example/project", "production-test")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c := &Component{Name: "worker", PolicyPath: filepath.Join(dir, "policy.json"), IdentityPath: filepath.Join(dir, "identity.json")}
	policy, _ := json.Marshal(material.Policy)
	identity, _ := json.Marshal(material.Identities["worker"])
	if err := os.WriteFile(c.PolicyPath, policy, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.IdentityPath, identity, 0600); err != nil {
		t.Fatal(err)
	}
	return c, material
}

func TestMountedAuthorityRejectsMissingWrongAndChangedIdentity(t *testing.T) {
	c, material := files(t)
	if _, _, err := c.Authority(); err != nil {
		t.Fatal(err)
	}
	wrong, _ := json.Marshal(material.Identities["relay"])
	if err := os.WriteFile(c.IdentityPath, wrong, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Authority(); err == nil {
		t.Fatal("wrong component Secret accepted")
	}
	if err := os.Remove(c.IdentityPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Authority(); err == nil {
		t.Fatal("missing Secret accepted")
	}
	if err := os.WriteFile(c.IdentityPath, []byte(strings.Repeat(" ", 128*1024+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Authority(); err == nil {
		t.Fatal("unbounded configuration accepted")
	}
}

func TestUnknownModesFailBeforeOpeningDatabase(t *testing.T) {
	for _, mode := range []string{"active", "observe", "typo"} {
		t.Setenv("AI_SDLC_RUNTIME_MODE", mode)
		if _, err := Open(context.Background(), "worker"); err == nil {
			t.Fatalf("unsupported mode %s started", mode)
		}
	}
	t.Setenv("AI_SDLC_RUNTIME_MODE", "")
	t.Setenv("DATABASE_URL", "")
	if _, err := Open(context.Background(), "worker"); err == nil {
		t.Fatal("missing store became a stateless service")
	}
}

func TestDedicatedStartupAndReadiness(t *testing.T) {
	databaseURL := os.Getenv("AI_SDLC_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("PostgreSQL required")
	}
	ctx := context.Background()
	admin, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := admin.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	passwords := map[string]string{}
	for _, role := range []string{"intake", "admission-controller", "orchestrator", "worker", "verifier", "release-manager", "relay", "deployer"} {
		passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("x", 40)
	}
	if err := admin.ProvisionRoles(ctx, passwords); err != nil {
		t.Fatal(err)
	}
	c, _ := files(t)
	t.Setenv("AI_SDLC_RUNTIME_MODE", "maintenance")
	t.Setenv("AI_SDLC_AUTHORITY_POLICY_PATH", c.PolicyPath)
	t.Setenv("AI_SDLC_IDENTITY_PATH", c.IdentityPath)
	t.Setenv("DATABASE_URL", databaseURL)
	if _, err := Open(ctx, "worker"); err == nil {
		t.Fatal("bootstrap database credential accepted")
	}
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("aisdlc_worker", passwords["worker"])
	t.Setenv("DATABASE_URL", u.String())
	component, err := Open(ctx, "worker")
	if err != nil {
		t.Fatal(err)
	}
	defer component.DB.Close()
	if err := component.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := component.DB.Pool.ExecContext(ctx, `CREATE TABLE forbidden_service_schema_change(id integer)`); err == nil {
		t.Fatal("service role could perform schema setup")
	}
	if err := os.Remove(c.PolicyPath); err != nil {
		t.Fatal(err)
	}
	if err := component.Ready(ctx); err == nil {
		t.Fatal("readiness ignored revoked policy mount")
	}
}

func TestCandidateCeilingCannotGrantPublication(t *testing.T) {
	t.Setenv("AI_SDLC_RUNTIME_MODE", "candidate")
	c, m := files(t)
	m.Policy.Autonomy = "auto-pr"
	write := func() {
		b, _ := json.Marshal(m.Policy)
		if e := os.WriteFile(c.PolicyPath, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	write()
	if _, _, e := c.Authority(); e == nil {
		t.Fatal("candidate accepted downstream grants")
	}
	var grants []authority.Grant
	for _, g := range m.Policy.Grants {
		if g.Operation == "admit-issue" || g.Operation == "plan-admission" || g.Operation == "execute-work" {
			grants = append(grants, g)
		}
	}
	m.Policy.Grants = grants
	write()
	if _, _, e := c.Authority(); e != nil {
		t.Fatal(e)
	}
	m.Policy.Autonomy = "auto-deploy"
	write()
	if _, _, e := c.Authority(); e == nil {
		t.Fatal("candidate accepted deploy ceiling")
	}
}

func TestVerificationCeilingRejectsPublicationAndCandidateDowngrade(t *testing.T) {
	t.Setenv("AI_SDLC_RUNTIME_MODE", "verification")
	c, m := files(t)
	m.Policy.Autonomy = "auto-pr"
	var grants []authority.Grant
	for _, g := range m.Policy.Grants {
		if g.Operation == "admit-issue" || g.Operation == "plan-admission" || g.Operation == "execute-work" || g.Operation == "verify-artifact" {
			grants = append(grants, g)
		}
	}
	all := m.Policy.Grants
	m.Policy.Grants = grants
	write := func() {
		b, _ := json.Marshal(m.Policy)
		if os.WriteFile(c.PolicyPath, b, 0600) != nil {
			t.Fatal("policy")
		}
	}
	write()
	if _, _, err := c.Authority(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_SDLC_RUNTIME_MODE", "candidate")
	if _, _, err := c.Authority(); err == nil {
		t.Fatal("candidate accepted verification")
	}
	t.Setenv("AI_SDLC_RUNTIME_MODE", "verification")
	m.Policy.Grants = all
	write()
	if _, _, err := c.Authority(); err == nil {
		t.Fatal("verification accepted publication grants")
	}
}

func TestExplicitPublicationCeilingStillRejectsMergeAndDeploy(t *testing.T) {
	t.Setenv("AI_SDLC_RUNTIME_MODE", "verification")
	t.Setenv("AI_SDLC_INDEPENDENT_REVIEW", "true")
	t.Setenv("AI_SDLC_PUBLICATION", "true")
	c, m := files(t)
	m.Policy.Autonomy = "auto-pr"
	var grants []authority.Grant
	for _, g := range m.Policy.Grants {
		if g.Operation == "admit-issue" || g.Operation == "plan-admission" || g.Operation == "execute-work" || g.Operation == "verify-artifact" || g.Operation == "publish-candidate" {
			grants = append(grants, g)
		}
	}
	all := m.Policy.Grants
	m.Policy.Grants = grants
	write := func() {
		raw, _ := json.Marshal(m.Policy)
		if os.WriteFile(c.PolicyPath, raw, 0600) != nil {
			t.Fatal("policy fixture")
		}
	}
	write()
	if _, _, e := c.Authority(); e != nil || !PublicationEnabled() {
		t.Fatal("explicit publication ceiling rejected", e)
	}
	for _, change := range []struct{ name, value string }{{"AI_SDLC_RUNTIME_MODE", "candidate"}, {"AI_SDLC_RUNTIME_MODE", "maintenance"}, {"AI_SDLC_INDEPENDENT_REVIEW", "false"}, {"AI_SDLC_PUBLICATION", "false"}, {"AI_SDLC_PUBLICATION", "unexpected"}} {
		t.Run(change.name+change.value, func(t *testing.T) {
			t.Setenv(change.name, change.value)
			if _, _, e := c.Authority(); e == nil || PublicationEnabled() {
				t.Fatal("invalid publication context accepted")
			}
		})
	}
	m.Policy.Grants = all
	write()
	if _, _, e := c.Authority(); e == nil {
		t.Fatal("publication option accepted other downstream grants")
	}
	m.Policy.Grants = grants
	m.Policy.Autonomy = "auto-deploy"
	write()
	if _, _, e := c.Authority(); e == nil {
		t.Fatal("publication option accepted deploy ceiling")
	}
}

func TestMergeRequiresExplicitPilotCapabilityAndCannotDeploy(t *testing.T) {
	t.Setenv("AI_SDLC_RUNTIME_MODE", "verification")
	t.Setenv("AI_SDLC_INDEPENDENT_REVIEW", "true")
	t.Setenv("AI_SDLC_PUBLICATION", "true")
	t.Setenv("AI_SDLC_MERGE", "true")
	c, m := files(t)
	m.Policy.Repository = "os-santiago/homedir"
	m.Policy.Autonomy = "auto-merge-low"
	var grants []authority.Grant
	for _, g := range m.Policy.Grants {
		if g.Operation == "admit-issue" || g.Operation == "plan-admission" || g.Operation == "execute-work" || g.Operation == "verify-artifact" || g.Operation == "publish-candidate" || g.Operation == "merge-publication" {
			grants = append(grants, g)
		}
	}
	m.Policy.Grants = grants
	write := func() {
		raw, _ := json.Marshal(m.Policy)
		if os.WriteFile(c.PolicyPath, raw, 0600) != nil {
			t.Fatal("policy")
		}
	}
	write()
	if _, _, e := c.Authority(); e != nil || !MergeEnabled() {
		t.Fatal("explicit merge denied", e)
	}
	for _, change := range []struct{ name, value string }{{"AI_SDLC_MERGE", "false"}, {"AI_SDLC_MERGE", "unexpected"}, {"AI_SDLC_PUBLICATION", "false"}, {"AI_SDLC_INDEPENDENT_REVIEW", "false"}, {"AI_SDLC_RUNTIME_MODE", "candidate"}} {
		t.Run(change.name+change.value, func(t *testing.T) {
			t.Setenv(change.name, change.value)
			if _, _, e := c.Authority(); e == nil || MergeEnabled() {
				t.Fatal("invalid merge context accepted")
			}
		})
	}
	m.Policy.Autonomy = "auto-deploy"
	write()
	if _, _, e := c.Authority(); e == nil {
		t.Fatal("merge accepted deployment ceiling")
	}
	m.Policy.Autonomy = "auto-merge-low"
	m.Policy.Repository = "os-santiago/ai-sdlc"
	write()
	if _, _, e := c.Authority(); e == nil {
		t.Fatal("merge escaped pilot")
	}
}
