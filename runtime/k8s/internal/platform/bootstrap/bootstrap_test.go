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
