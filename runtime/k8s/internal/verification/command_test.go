package verification

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

func TestSignedCommandBindsWorkerCandidateAndCurrentRecipe(t *testing.T) {
	material, err := authority.Bootstrap("os-santiago/homedir", "production-test")
	if err != nil {
		t.Fatal(err)
	}
	p := material.Policy
	p.Freeze = false
	p.Autonomy = "auto-pr"
	now := time.Now()
	original := Command{Version: CommandVersion, CandidateID: "execution-" + strings.Repeat("a", 64), WorkItemID: "11111111-1111-4111-8111-111111111111", Repository: p.Repository, IssueNumber: 5, Generation: 1, IssueRevision: strings.Repeat("b", 64), SourceSHA: strings.Repeat("c", 40), DeltaSHA256: strings.Repeat("d", 64), CandidateTreeSHA256: strings.Repeat("e", 64), Recipe: LocaleRecipe, RecipeRevision: RecipeRevision(), AuthorityRevision: p.Revision(), ExpiresAt: now.Add(5 * time.Minute).UnixMilli()}
	for name, mutate := range map[string]func(*Command){"valid": func(*Command) {}, "wrong repository": func(c *Command) { c.Repository = "other/repo" }, "missing issue binding": func(c *Command) { c.IssueRevision = "" }, "stale recipe": func(c *Command) { c.RecipeRevision = strings.Repeat("a", 64) }, "unsupported recipe": func(c *Command) { c.Recipe = "arbitrary-command" }, "expired": func(c *Command) { c.ExpiresAt = now.Add(-time.Second).UnixMilli() }, "unbounded": func(c *Command) { c.ExpiresAt = now.Add(time.Hour).UnixMilli() }} {
		t.Run(name, func(t *testing.T) {
			c := original
			mutate(&c)
			raw, _ := json.Marshal(c)
			receipt, err := material.Identities["worker"].Seal(p, c.ID(), "verify-artifact", raw)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Verify(p, receipt, raw, now)
			if (err == nil) != (name == "valid") {
				t.Fatalf("command decision %v", err)
			}
			if name == "valid" {
				changed := p
				changed.Freeze = true
				if _, err = Verify(changed, receipt, raw, now); err == nil {
					t.Fatal("revoked policy admitted handoff")
				}
				receipt.Producer = "orchestrator"
				if _, err = Verify(p, receipt, raw, now); err == nil {
					t.Fatal("producer identity substituted")
				}
			}
		})
	}
}
