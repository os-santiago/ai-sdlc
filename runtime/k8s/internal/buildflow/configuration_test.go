package buildflow

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installedFixture(t *testing.T) Installed {
	t.Helper()
	m, e := authority.Bootstrap("os-santiago/saferus", "ai-sdlc-production")
	if e != nil {
		t.Fatal(e)
	}
	inner, outer := m.Policy, m.Policy
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
		case "build-controller", "release-manager", "verifier", "deployer", "relay", "orchestrator":
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
	p := buildworkload.Profile{Version: "ai-sdlc.saferus-build-profile/v1", ClusterUID: uid, NamespaceUID: uid, PublisherImage: "localhost/ai-sdlc-artifact-builder@sha256:" + strings.Repeat("a", 64), Dependencies: map[string]buildworkload.Dependency{}, DeniedTargets: []buildworkload.DeniedTarget{{Host: "10.96.0.1", Port: 443}, {Host: "10.96.0.2", Port: 443}}}
	for _, k := range buildworkload.DependencyKeys() {
		p.Dependencies[k] = buildworkload.Dependency{UID: uid, SpecSHA256: strings.Repeat("b", 64)}
	}
	s := building.Slot{ClusterUID: uid, NamespaceUID: uid, PVCUID: uid, PVUID: uid, NodeUID: uid, NodeBootID: uid, StorageIdentitySHA256: strings.Repeat("c", 64), Namespace: "ai-sdlc-builds", PVC: "slot", Node: "node", CapacityBytes: 4 << 30, InodeLimit: 32768}
	return Installed{Version: "ai-sdlc.build-installation/v1", Inner: inner, Outer: outer, Profile: p, Slots: []building.Slot{s}, IntakeConfigSHA256: strings.Repeat("d", 64), IssueMaxAgeSeconds: 60}
}
func TestInstalledBuildRequiresExplicitLoopPolicyFields(t *testing.T) {
	original := installedFixture(t)
	raw, _ := json.Marshal(original)
	path := filepath.Join(t.TempDir(), "installed.json")
	if e := os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	loaded, e := LoadInstalled(path)
	if e != nil || loaded.Digest() != original.Digest() {
		t.Fatal("explicit frozen installation rejected", e)
	}
	for _, loop := range []string{"inner", "outer"} {
		for _, field := range []string{"freeze", "grants", "keys", "repository", "environment", "autonomy", "version"} {
			t.Run(loop+"-"+field, func(t *testing.T) {
				var obj map[string]any
				if json.Unmarshal(raw, &obj) != nil {
					t.Fatal("fixture")
				}
				delete(obj[loop].(map[string]any), field)
				bad, _ := json.Marshal(obj)
				if e := os.WriteFile(path, bad, 0600); e != nil {
					t.Fatal(e)
				}
				if _, e := LoadInstalled(path); e == nil {
					t.Fatal("missing explicit authority field accepted")
				}
			})
		}
	}
}
