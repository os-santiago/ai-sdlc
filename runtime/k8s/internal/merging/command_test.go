package merging

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (authority.BootstrapMaterial, Command, time.Time) {
	t.Helper()
	m, e := authority.Bootstrap("os-santiago/homedir", "ai-sdlc-production")
	if e != nil {
		t.Fatal(e)
	}
	m.Policy.Autonomy = "auto-merge-low"
	m.Policy.Freeze = false
	now := time.Now()
	d := strings.Repeat("a", 64)
	c := Command{Version: Version, PublicationID: "publication-" + d, PublicationRecordSHA256: d, GateSHA256: d, Repository: m.Policy.Repository, IssueNumber: 3, PullRequest: 42, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), TreeSHA: strings.Repeat("c", 40), Branch: "ai-sdlc/issue-3-" + strings.Repeat("d", 32), AuthorityRevision: m.Policy.Revision(), IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(MaxAge).UnixMilli()}
	return m, c, now
}
func TestFreshMergeRequiresSeparateCapabilityAndCurrentBound(t *testing.T) {
	m, c, now := fixture(t)
	raw, _ := json.Marshal(c)
	receipt, e := m.Identities["release-manager"].Seal(m.Policy, c.ID(), "merge-publication", raw)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Verify(m.Policy, receipt, raw, now); e != nil {
		t.Fatal(e)
	}
	for _, role := range []string{"worker", "verifier", "orchestrator", "intake", "relay", "deployer"} {
		if _, e = m.Identities[role].Seal(m.Policy, c.ID(), "merge-publication", raw); e == nil {
			t.Fatal("wrong merge owner", role)
		}
	}
	for _, level := range []string{"shadow", "suggest", "auto-pr", "auto-merge-all", "auto-deploy"} {
		p := m.Policy
		p.Autonomy = level
		if _, e = Verify(p, receipt, raw, now); e == nil {
			t.Fatal("unreviewed autonomy", level)
		}
	}
	frozen := m.Policy
	frozen.Freeze = true
	if _, e = Verify(frozen, receipt, raw, now); e == nil {
		t.Fatal("frozen merge")
	}
	if _, e = Verify(m.Policy, receipt, raw, now.Add(MaxAge)); e == nil {
		t.Fatal("expired merge authority")
	}
	old := m.Policy
	old.Grants = append([]authority.Grant(nil), old.Grants...)
	for i := range old.Grants {
		if old.Grants[i].Operation == "merge-publication" {
			old.Grants[i].MinimumLevel = "auto-pr"
		}
	}
	if old.Validate() == nil {
		t.Fatal("merge floor weakened")
	}
}
func TestMergeCommandCannotCarryUnreviewedDestinationOrInstructions(t *testing.T) {
	m, c, now := fixture(t)
	for _, change := range []func(*Command){func(v *Command) { v.HeadSHA = "main" }, func(v *Command) { v.Branch = "main" }, func(v *Command) { v.PullRequest = 0 }, func(v *Command) { v.PublicationRecordSHA256 = "" }, func(v *Command) { v.GateSHA256 = "" }, func(v *Command) { v.Repository = "other/repo" }, func(v *Command) { v.ExpiresAt = now.Add(3 * time.Minute).UnixMilli() }, func(v *Command) { v.IssuedAt = now.Add(time.Minute).UnixMilli() }, func(v *Command) { v.ExpiresAt = now.Add(-time.Second).UnixMilli() }} {
		altered := c
		change(&altered)
		raw, _ := json.Marshal(altered)
		receipt, e := m.Identities["release-manager"].Seal(m.Policy, altered.ID(), "merge-publication", raw)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = Verify(m.Policy, receipt, raw, now); e == nil {
			t.Fatal("unsafe command accepted")
		}
	}
	raw, _ := json.Marshal(c)
	for _, field := range []string{`"endpoint":"https://untrusted.invalid"`, `"mergeMethod":"rebase"`, `"admin":true`, `"deploy":true`, `"command":"exec"`, `"expiresAt":null`} {
		altered := append(append([]byte(nil), raw[:len(raw)-1]...), []byte(","+field+"}")...)
		receipt, e := m.Identities["release-manager"].Seal(m.Policy, c.ID(), "merge-publication", altered)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = Verify(m.Policy, receipt, altered, now); e == nil {
			t.Fatal("extra capability accepted")
		}
	}
}
