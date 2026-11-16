package main

import (
	"bytes"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"strings"
	"testing"
)

func installedFixture(t *testing.T) buildflow.Installed {
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
		case "build-controller", "release-manager", "verifier", "deployer", "relay":
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
	return buildflow.Installed{Version: "ai-sdlc.build-installation/v1", Inner: inner, Outer: outer, Profile: p, Slots: []building.Slot{s}, IntakeConfigSHA256: strings.Repeat("d", 64), IssueMaxAgeSeconds: 60}
}

func TestBuildInstallationCompilerBindsCurrentIntakeAndPolicies(t *testing.T) {
	installed := installedFixture(t)
	config := intake.Config{Version: 1, Repository: "os-santiago/saferus", BaseBranch: "main", TriggerLabel: "ai-sdlc:shadow", AuthorizedActorIDs: []int64{42}, PollSeconds: 30, MaxPages: 100}
	input := map[string]any{"inner": installed.Inner, "outer": installed.Outer, "profile": installed.Profile, "slots": installed.Slots, "intake": config, "issueMaxAgeSeconds": 60}
	raw, _ := json.Marshal(input)
	var out bytes.Buffer
	if e := compileBuildInstallation(bytes.NewReader(raw), &out); e != nil {
		t.Fatal(e)
	}
	var actual buildflow.Installed
	if json.Unmarshal(out.Bytes(), &actual) != nil || actual.Validate() != nil || actual.IntakeConfigSHA256 != config.Digest() || actual.Inner.Revision() != installed.Inner.Revision() || actual.Outer.Revision() != installed.Outer.Revision() {
		t.Fatal("installation compiler lost approved scope")
	}
	for _, name := range []string{"unknown", "foreign-intake", "implicit-freeze", "slot-drift", "unbounded-age"} {
		t.Run(name, func(t *testing.T) {
			var changed map[string]any
			json.Unmarshal(raw, &changed)
			switch name {
			case "unknown":
				changed["run"] = true
			case "foreign-intake":
				changed["intake"].(map[string]any)["repository"] = "os-santiago/homedir"
			case "implicit-freeze":
				delete(changed["outer"].(map[string]any), "freeze")
			case "slot-drift":
				changed["slots"].([]any)[0].(map[string]any)["clusterUID"] = "22222222-2222-2222-2222-222222222222"
			case "unbounded-age":
				changed["issueMaxAgeSeconds"] = 301
			}
			altered, _ := json.Marshal(changed)
			out.Reset()
			if compileBuildInstallation(bytes.NewReader(altered), &out) == nil || out.Len() != 0 {
				t.Fatal("invalid scope produced installation")
			}
		})
	}
}
