package publication

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
)

func fixture(t *testing.T) (authority.BootstrapMaterial, Command, verification.Command, time.Time) {
	t.Helper()
	m, err := authority.Bootstrap("os-santiago/homedir", "ai-sdlc-production")
	if err != nil {
		t.Fatal(err)
	}
	m.Policy.Autonomy = "auto-pr"
	m.Policy.Freeze = false
	now := time.Now()
	d := strings.Repeat("a", 64)
	v := verification.Command{Version: verification.CommandVersion, CandidateID: "execution-" + d, WorkItemID: "11111111-1111-4111-8111-111111111111", Repository: m.Policy.Repository, IssueNumber: 3, Generation: 1, IssueRevision: d, SourceSHA: strings.Repeat("b", 40), DeltaSHA256: d, CandidateTreeSHA256: d, Recipe: verification.LocaleRecipe, RecipeRevision: verification.RecipeRevision(), AuthorityRevision: m.Policy.Revision(), ExpiresAt: now.Add(10 * time.Minute).UnixMilli()}
	c := Command{Version: Version, VerificationID: v.ID(), CandidateID: v.CandidateID, WorkItemID: v.WorkItemID, Repository: v.Repository, IssueNumber: v.IssueNumber, Generation: v.Generation, IssueRevision: v.IssueRevision, SourceSHA: v.SourceSHA, DeltaSHA256: v.DeltaSHA256, CandidateTreeSHA256: v.CandidateTreeSHA256, BehaviorProofSHA256: d, IndependentReviewSHA256: d, ReviewerProfileSHA256: d, AuthorityRevision: v.AuthorityRevision, ExpiresAt: v.ExpiresAt}
	return m, c, v, now
}

func TestPublicationRequiresCurrentIndependentSignerAndEvidenceBindings(t *testing.T) {
	m, c, v, now := fixture(t)
	raw, _ := json.Marshal(c)
	receipt, err := m.Identities["verifier"].Seal(m.Policy, c.ID(), "publish-candidate", raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(m.Policy, receipt, raw, now); err != nil || MatchVerification(c, v) != nil {
		t.Fatal("valid signed publication binding", err)
	}
	for _, role := range []string{"worker", "orchestrator", "release-manager"} {
		if _, err = m.Identities[role].Seal(m.Policy, c.ID(), "publish-candidate", raw); err == nil {
			t.Fatal("unauthorized publisher", role)
		}
	}
	frozen := m.Policy
	frozen.Freeze = true
	if _, err = Verify(frozen, receipt, raw, now); err == nil {
		t.Fatal("revoked publication accepted")
	}
	modified := bytes.Replace(raw, []byte(c.SourceSHA), []byte(strings.Repeat("c", 40)), 1)
	if _, err = Verify(m.Policy, receipt, modified, now); err == nil {
		t.Fatal("altered signed bytes")
	}
	for _, mutate := range []func(*Command){
		func(c *Command) { c.Repository = "other/repository" }, func(c *Command) { c.IndependentReviewSHA256 = "" },
		func(c *Command) { c.ReviewerProfileSHA256 = "" }, func(c *Command) { c.BehaviorProofSHA256 = "" },
		func(c *Command) { c.CandidateID = "" }, func(c *Command) { c.VerificationID = "" },
		func(c *Command) { c.ExpiresAt = now.Add(-time.Second).UnixMilli() }, func(c *Command) { c.ExpiresAt = now.Add(time.Hour).UnixMilli() },
	} {
		changed := c
		mutate(&changed)
		payload, _ := json.Marshal(changed)
		signed, e := m.Identities["verifier"].Seal(m.Policy, changed.ID(), "publish-candidate", payload)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = Verify(m.Policy, signed, payload, now); e == nil {
			t.Fatal("invalid publication command accepted")
		}
	}
	for _, mutate := range []func(*verification.Command){
		func(v *verification.Command) { v.SourceSHA = strings.Repeat("c", 40) }, func(v *verification.Command) { v.Generation++ },
		func(v *verification.Command) { v.DeltaSHA256 = strings.Repeat("d", 64) }, func(v *verification.Command) { v.ExpiresAt-- },
		func(v *verification.Command) { v.Recipe = "unreviewed" },
	} {
		changed := v
		mutate(&changed)
		if MatchVerification(c, changed) == nil {
			t.Fatal("verification substituted")
		}
	}
}

func TestPublicationCannotCarryDestinationOrExecutableInstructions(t *testing.T) {
	m, c, _, now := fixture(t)
	raw, _ := json.Marshal(c)
	for _, field := range []string{`"branch":"main"`, `"endpoint":"https://example.com"`, `"command":"exec"`, `"skipReview":true`, `"merge":true`, `"deploy":true`, `"expiresAt":null`} {
		payload := append(append([]byte(nil), raw[:len(raw)-1]...), []byte(","+field+"}")...)
		signed, err := m.Identities["verifier"].Seal(m.Policy, c.ID(), "publish-candidate", payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = Verify(m.Policy, signed, payload, now); err == nil {
			t.Fatal("extra capability accepted")
		}
	}
}
