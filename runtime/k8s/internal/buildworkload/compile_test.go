package buildworkload

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
)

func setup(t *testing.T) (building.Configuration, Profile, building.Command, authority.Identity) {
	t.Helper()
	m, e := authority.Bootstrap("os-santiago/saferus", "ai-sdlc-production")
	if e != nil {
		t.Fatal(e)
	}
	inner, outer := m.Policy, m.Policy
	inner.Freeze = false
	outer.Freeze = false
	inner.Autonomy = "auto-merge-low"
	outer.Autonomy = "auto-deploy"
	inner.Keys = map[string]authority.PublicKey{}
	outer.Keys = map[string]authority.PublicKey{}
	inner.Grants = nil
	outer.Grants = nil
	for id, k := range m.Policy.Keys {
		if k.Role != "build-controller" {
			inner.Keys[id] = k
		}
		switch k.Role {
		case "release-manager", "build-controller", "verifier", "deployer", "relay", "orchestrator":
			outer.Keys[id] = k
		}
	}
	for _, g := range m.Policy.Grants {
		if authority.OuterOperation(g.Operation) {
			outer.Grants = append(outer.Grants, g)
		} else if g.Operation != "record-attempt" && g.Operation != "record-release" {
			inner.Grants = append(inner.Grants, g)
		}
	}
	uid := "11111111-1111-1111-1111-111111111111"
	s := building.Slot{ClusterUID: uid, NamespaceUID: uid, PVCUID: uid, PVUID: uid, NodeUID: uid, NodeBootID: uid, StorageIdentitySHA256: strings.Repeat("a", 64), Namespace: Namespace, PVC: "build-slot-0", Node: "fixture-node", CapacityBytes: 4 << 30, InodeLimit: 32768}
	p := Profile{Version: "ai-sdlc.saferus-build-profile/v1", ClusterUID: uid, NamespaceUID: uid, PublisherImage: "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("b", 64), Dependencies: map[string]Dependency{}, DeniedTargets: []DeniedTarget{{"10.96.0.1", 443}, {"10.96.0.2", 443}}}
	for _, k := range required {
		p.Dependencies[k] = Dependency{uid, strings.Repeat("c", 64)}
	}
	cfg := building.Configuration{Inner: inner, Outer: outer, ProfileSHA256: p.Digest(), IntakeConfigSHA256: strings.Repeat("d", 64), IssueMaxAgeSeconds: 60, Slots: []building.Slot{s}}
	now := time.Now()
	c := building.Command{Version: building.Version, MergeID: "merge-" + strings.Repeat("e", 64), MergeRecordSHA256: strings.Repeat("f", 64), Repository: inner.Repository, Environment: inner.Environment, IssueNumber: 3, IssueRevision: strings.Repeat("a", 64), Generation: 1, SourceSHA: strings.Repeat("b", 40), TreeSHA: strings.Repeat("c", 40), Recipe: building.Recipe, ProfileSHA256: cfg.ProfileDigest(), IntakeConfigSHA256: cfg.IntakeConfigSHA256, InnerPolicyRevision: inner.Revision(), OuterPolicyRevision: outer.Revision(), IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(building.MaxAge).UnixMilli()}
	return cfg, p, c, m.Identities["release-manager"]
}
func compile(t *testing.T, cfg building.Configuration, p Profile, c building.Command, key authority.Identity) (Compiled, error) {
	t.Helper()
	raw, _ := json.Marshal(c)
	signed, e := key.Seal(cfg.Outer, c.ID(), "build-merged", raw)
	if e != nil {
		t.Fatal(e)
	}
	return Compile(cfg, p, signed, raw, cfg.Slots[0], time.Now())
}
func TestClosedBuildPlanBindsExactBytesAndRejectsExecutableInputs(t *testing.T) {
	cfg, p, c, key := setup(t)
	w, e := compile(t, cfg, p, c, key)
	if e != nil {
		t.Fatal(e)
	}
	if w.Plan.BuildRequestSHA256 != authority.Digest(w.Build) || w.Plan.PublishRequestSHA256 != authority.Digest(w.Publish) || w.Plan.Validate(c, cfg.Slots[0]) != nil {
		t.Fatal("unbound exact requests")
	}
	for _, raw := range [][]byte{w.Build, w.Publish} {
		var obj map[string]any
		if json.Unmarshal(raw, &obj) != nil {
			t.Fatal("invalid compiled JSON")
		}
		metadata := obj["metadata"].(map[string]any)
		for _, v := range metadata["labels"].(map[string]any) {
			if len(v.(string)) > 63 {
				t.Fatal("invalid Kubernetes label")
			}
		}
	}
	build, pub := string(w.Build), string(w.Publish)
	for _, bad := range []string{`"secret":`, `"hostPath":`, `"projected":`, `"fsGroup":`, `"taskRef":`, `"resolver":`, `"retries":`} {
		if strings.Contains(build, bad) {
			t.Fatal("build acquired ambient capability", bad)
		}
	}
	if !strings.Contains(build, "--guard-workspace") || !strings.Contains(build, c.SourceSHA) || !strings.Contains(build, c.TreeSHA) || !strings.Contains(build, GoImage) || !strings.Contains(build, "10.96.0.1") {
		t.Fatal("closed build recipe incomplete")
	}
	if !strings.Contains(pub, "--input=/slot/workspace/output") || !strings.Contains(pub, "--definition-digest=sha256:"+w.Plan.DefinitionSHA256) || !strings.Contains(pub, `"readOnly":true`) {
		t.Fatal("publisher not bound")
	}
	again, e := compile(t, cfg, p, c, key)
	if e != nil || string(w.Build) != string(again.Build) || string(w.Publish) != string(again.Publish) {
		t.Fatal("compile not deterministic")
	}
	if w.MayCreate("publish", w.ExpiresAt.Add(-PublishBudget)) == nil || w.MayCreate("repair", time.Now()) == nil {
		t.Fatal("insufficient budget or unknown phase accepted")
	}
}
func TestClosedBuildProfileAndAuthorityDenials(t *testing.T) {
	for _, name := range []string{"publisher-tag", "publisher-host", "missing-network", "public-network", "duplicate-network", "missing-dependency", "wrong-UID", "unknown-dependency", "changed-profile", "changed-slot", "freeze", "short-deadline", "source-injection", "tree-injection", "wrong-recipe"} {
		t.Run(name, func(t *testing.T) {
			cfg, p, c, key := setup(t)
			switch name {
			case "publisher-tag":
				p.PublisherImage = "quay.io/os-santiago/ai-sdlc:latest"
			case "publisher-host":
				p.PublisherImage = "quayXio/os-santiago/ai-sdlc@sha256:" + strings.Repeat("a", 64)
			case "missing-network":
				p.DeniedTargets = nil
			case "public-network":
				p.DeniedTargets[0].Host = "8.8.8.8"
			case "duplicate-network":
				p.DeniedTargets[1] = p.DeniedTargets[0]
			case "missing-dependency":
				delete(p.Dependencies, required[0])
			case "wrong-UID":
				p.Dependencies[required[0]] = Dependency{"name-not-uid", strings.Repeat("a", 64)}
			case "unknown-dependency":
				p.Dependencies["pod/extra"] = Dependency{"11111111-1111-1111-1111-111111111111", strings.Repeat("a", 64)}
			case "changed-profile":
				cfg.ProfileSHA256 = strings.Repeat("0", 64)
			case "changed-slot":
				cfg.Slots[0].StorageIdentitySHA256 = strings.Repeat("0", 64)
			case "freeze":
				cfg.Inner.Freeze = true
			case "short-deadline":
				c.ExpiresAt = time.Now().Add(15 * time.Minute).UnixMilli()
			case "source-injection":
				c.SourceSHA = "main;echo injected"
			case "tree-injection":
				c.TreeSHA = "$(touch /bad)"
			case "wrong-recipe":
				c.Recipe = "arbitrary-shell"
			}
			if _, e := compile(t, cfg, p, c, key); e == nil {
				t.Fatal("bad workload input accepted")
			}
		})
	}
}

// Produces only synthetic signed manifests for server-side dry-run validation.
// No private key is exported and no live issue or runtime journal is touched.
func TestEmitClosedBuildAPIFixture(t *testing.T) {
	directory := os.Getenv("AI_SDLC_BUILD_COMPILER_OUTPUT")
	if directory == "" {
		t.Skip("explicit API dry-run fixture required")
	}
	cfg, p, c, key := setup(t)
	w, e := compile(t, cfg, p, c, key)
	if e != nil {
		t.Fatal(e)
	}
	policy, binding, e := Admission(p, cfg.Slots)
	if e != nil {
		t.Fatal(e)
	}
	mutation, mutationBinding := BoundsMutation()
	for name, raw := range map[string][]byte{"mutation.json": mutation, "mutation-binding.json": mutationBinding, "build.json": w.Build, "publish.json": w.Publish, "policy.json": policy, "binding.json": binding} {
		f, e := os.OpenFile(filepath.Join(directory, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.Write(raw); e != nil {
			t.Fatal(e)
		}
		if e = f.Close(); e != nil {
			t.Fatal(e)
		}
	}
}

func TestCELLiteralsPreserveQuotesAndCannotIntroduceApostrophes(t *testing.T) {
	for _, value := range []any{"' || true || '", "\\'\"\\u0027", []string{"safe", "'\\'"}, map[string]string{"'key": "value'"}} {
		literal := cel(value)
		if strings.Contains(literal, "'") {
			t.Fatal("raw apostrophe escaped literal boundary")
		}
		original, _ := json.Marshal(value)
		var decoded any
		if json.Unmarshal([]byte(literal), &decoded) != nil {
			t.Fatal("not a complete JSON literal")
		}
		roundtrip, _ := json.Marshal(decoded)
		if string(roundtrip) != string(original) {
			t.Fatal("literal escaping changed meaning")
		}
	}
}
