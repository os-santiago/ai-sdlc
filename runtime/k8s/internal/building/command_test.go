package building

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (authority.Policy, authority.Policy, map[string]authority.Identity, Command, time.Time) {
	t.Helper()
	m, e := authority.Bootstrap("os-santiago/saferus", "ai-sdlc-production")
	if e != nil {
		t.Fatal(e)
	}
	inner := m.Policy
	inner.Autonomy = "auto-merge-low"
	inner.Freeze = false
	inner.Grants = nil
	outer := m.Policy
	outer.Autonomy = "auto-deploy"
	outer.Freeze = false
	outer.Grants = nil
	outer.Keys = map[string]authority.PublicKey{}
	for _, g := range m.Policy.Grants {
		if authority.OuterOperation(g.Operation) {
			outer.Grants = append(outer.Grants, g)
		} else if g.Operation != "record-attempt" && g.Operation != "record-release" {
			inner.Grants = append(inner.Grants, g)
		}
	}
	for id, k := range inner.Keys {
		switch k.Role {
		case "release-manager", "build-controller", "verifier", "deployer", "relay", "orchestrator":
			outer.Keys[id] = k
		}
	}
	delete(inner.Keys, "build-controller-1")
	now := time.Now().UTC().Truncate(time.Millisecond)
	c := Command{Version: Version, MergeID: "merge-" + strings.Repeat("a", 64), MergeRecordSHA256: strings.Repeat("b", 64), Repository: inner.Repository, Environment: inner.Environment, IssueNumber: 3, IssueRevision: strings.Repeat("c", 64), Generation: 1, SourceSHA: strings.Repeat("d", 40), TreeSHA: strings.Repeat("e", 40), Recipe: Recipe, ProfileSHA256: strings.Repeat("f", 64), IntakeConfigSHA256: strings.Repeat("1", 64), InnerPolicyRevision: inner.Revision(), OuterPolicyRevision: outer.Revision(), IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(15 * time.Minute).UnixMilli()}
	return inner, outer, m.Identities, c, now
}
func TestBuildAuthorityAndCrossLoopIsolation(t *testing.T) {
	inner, outer, ids, c, now := fixture(t)
	raw, _ := json.Marshal(c)
	r, e := ids["release-manager"].Seal(outer, c.ID(), "build-merged", raw)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Verify(inner, outer, r, raw, now); e != nil {
		t.Fatal(e)
	}
	if inner.VerifyReceipt(r, "build-controller", "build-merged", raw) == nil {
		t.Fatal("outer receipt accepted by inner policy")
	}
	for _, role := range []string{"worker", "verifier", "relay", "deployer", "build-controller"} {
		if _, e := ids[role].Seal(outer, c.ID(), "build-merged", raw); e == nil {
			t.Fatal("wrong producer sealed", role)
		}
	}
}
func TestBuildAuthorityDenials(t *testing.T) {
	for _, name := range []string{"inner-freeze", "outer-freeze", "inner-revision", "outer-revision", "foreign-repo", "foreign-env", "expiry", "future", "long-life", "bad-merge", "bad-source", "bad-profile", "wrong-recipe", "wrong-generation", "inner-grant", "outer-grant", "rotated-shared-key", "builder-inner-key", "missing-relay"} {
		t.Run(name, func(t *testing.T) {
			inner, outer, ids, c, now := fixture(t)
			raw, _ := json.Marshal(c)
			r, e := ids["release-manager"].Seal(outer, c.ID(), "build-merged", raw)
			if e != nil {
				t.Fatal(e)
			}
			switch name {
			case "inner-freeze":
				inner.Freeze = true
			case "outer-freeze":
				outer.Freeze = true
			case "inner-revision":
				c.InnerPolicyRevision = strings.Repeat("0", 64)
			case "outer-revision":
				c.OuterPolicyRevision = strings.Repeat("0", 64)
			case "foreign-repo":
				c.Repository = "os-santiago/homedir"
			case "foreign-env":
				c.Environment = "production"
			case "expiry":
				c.ExpiresAt = now.UnixMilli()
			case "future":
				c.IssuedAt = now.Add(time.Minute).UnixMilli()
			case "long-life":
				c.ExpiresAt = now.Add(time.Hour).UnixMilli()
			case "bad-merge":
				c.MergeID = "merge-arbitrary"
			case "bad-source":
				c.SourceSHA = "main"
			case "bad-profile":
				c.ProfileSHA256 = "unbound"
			case "wrong-recipe":
				c.Recipe = "arbitrary"
			case "wrong-generation":
				c.Generation = 0
			case "inner-grant":
				inner.Grants = append(inner.Grants, outer.Grants[0])
			case "outer-grant":
				outer.Grants = append(outer.Grants, inner.Grants[0])
			case "rotated-shared-key":
				_, k, _ := authority.NewIdentity("verifier", "verifier-1")
				outer.Keys["verifier-1"] = k
			case "builder-inner-key":
				inner.Keys["build-controller-1"] = outer.Keys["build-controller-1"]
			case "missing-relay":
				delete(outer.Keys, "relay-1")
			}
			// Re-sign invalid commands when policy remains valid: validation must reject
			// even a signed producer request with bad bounds or mismatched bindings.
			raw, _ = json.Marshal(c)
			if signed, err := ids["release-manager"].Seal(outer, c.ID(), "build-merged", raw); err == nil {
				r = signed
			}
			if _, e = Verify(inner, outer, r, raw, now); e == nil {
				t.Fatal("invalid authority accepted")
			}
		})
	}
}
func TestBuildCommandRejectsAmbiguousAndUnknownFields(t *testing.T) {
	inner, outer, ids, c, now := fixture(t)
	raw, _ := json.Marshal(c)
	for _, extra := range []string{`,"sourceSHA":"main"}`, `,"command":"curl arbitrary"}`} {
		changed := append(append([]byte{}, raw[:len(raw)-1]...), []byte(extra)...)
		r, e := ids["release-manager"].Seal(outer, c.ID(), "build-merged", changed)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = Verify(inner, outer, r, changed, now); e == nil {
			t.Fatal("ambiguous command accepted")
		}
	}
}

func TestBuilderCannotAliasAnyInnerOnlyIdentity(t *testing.T) {
	for _, role := range []string{"intake", "admission-controller", "orchestrator", "worker"} {
		t.Run(role, func(t *testing.T) {
			inner, outer, _, _, _ := fixture(t)
			key := inner.Keys[role+"-1"]
			key.Role = "build-controller"
			outer.Keys["build-controller-1"] = key
			if inner.Validate() != nil || (role != "orchestrator" && outer.Validate() != nil) {
				t.Fatal("fixture must isolate cross-policy alias")
			}
			if authority.ValidateLoopPolicies(inner, outer) == nil {
				t.Fatal("cross-policy role alias accepted")
			}
		})
	}
}
