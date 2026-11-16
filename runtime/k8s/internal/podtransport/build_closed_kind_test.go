package podtransport

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildstorage"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"golang.org/x/sys/unix"
)

// This explicitly isolated test bootstraps synthetic signing authority. It does
// not read or write a runtime journal, issue, release record or deployment.
func TestRealClosedBuildTransport(t *testing.T) {
	path := os.Getenv("AI_SDLC_CLOSED_BUILD_FIXTURE")
	if path == "" {
		t.Skip("explicit isolated integration fixture required")
	}
	var input struct {
		Config                                                     Config
		Slot                                                       building.Slot
		Phase, OutputDirectory, PublisherImage, SourceSHA, TreeSHA string
	}
	raw, e := os.ReadFile(path)
	if e != nil || len(raw) > 256*1024 || authority.DecodeStrict(raw, &input) != nil || input.Slot.Validate() != nil {
		t.Fatal("private fixture input")
	}
	client, e := NewClient(input.Config)
	if e != nil {
		t.Fatal("private fixture client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 17*time.Minute)
	defer cancel()
	r := &BuildResources{client: client, slot: input.Slot, compiled: buildworkload.Compiled{ExpiresAt: time.Now().Add(18 * time.Minute)}}
	write := func(name string, value any) {
		body, e := json.MarshalIndent(value, "", "  ")
		if e != nil {
			t.Fatal("report encoding")
		}
		if os.WriteFile(filepath.Join(input.OutputDirectory, name), append(body, '\n'), 0600) != nil {
			t.Fatal("private report write")
		}
	}
	writeRaw := func(name string, body []byte) {
		if os.WriteFile(filepath.Join(input.OutputDirectory, name), body, 0600) != nil {
			t.Fatal("fixture definition write")
		}
	}
	p := buildworkload.Profile{Version: "ai-sdlc.saferus-build-profile/v1", ClusterUID: input.Slot.ClusterUID, NamespaceUID: input.Slot.NamespaceUID, PublisherImage: input.PublisherImage, Dependencies: map[string]buildworkload.Dependency{}}
	objects := map[string]map[string]any{}
	for _, key := range []string{"default/service/kubernetes", "default/endpoints/kubernetes", "kind-registry/service/registry-service", "kind-registry/endpoints/registry-service"} {
		path, _ := dependencyPath(key)
		obj, _, e := r.getBuildObject(ctx, path)
		if e != nil {
			t.Fatal("actual network inventory unavailable")
		}
		objects[key] = obj
	}
	p.DeniedTargets, e = BuildNetworkTargets(objects)
	if e != nil {
		t.Fatal("actual network targets invalid")
	}
	for _, key := range buildworkload.DependencyKeys() {
		p.Dependencies[key] = buildworkload.Dependency{UID: input.Slot.NamespaceUID, SpecSHA256: strings.Repeat("a", 64)}
	}
	if input.Phase == "definitions" {
		prepareClosedFixtureSlot(t, input.Slot)
		policy, binding, e := buildworkload.Admission(p, []building.Slot{input.Slot})
		if e != nil {
			t.Fatal("admission compilation")
		}
		mutation, mb := buildworkload.BoundsMutation()
		for name, body := range map[string][]byte{"policy.json": policy, "binding.json": binding, "mutation.json": mutation, "mutation-binding.json": mb} {
			writeRaw(name, body)
		}
		return
	}
	if input.Phase == "trust" {
		trust := BuildTrust{Version: "ai-sdlc.build-trust/v1", Components: map[string]buildworkload.Dependency{}}
		for key, path := range buildTrustPaths {
			_, body, e := r.getBuildObject(ctx, path)
			if e != nil {
				t.Fatal("actual trust component unavailable", key)
			}
			d, e := BuildObjectFingerprint(body)
			if e != nil {
				t.Fatal("actual trust component invalid", key)
			}
			trust.Components[key] = d
		}
		inventory := map[string][]map[string]any{}
		for _, path := range buildMutationPaths {
			items, e := r.listBuildObjects(ctx, path)
			if e != nil {
				t.Fatal("actual mutation inventory unavailable")
			}
			inventory[path] = items
		}
		trust.MutationInventorySHA256, e = mutationDigest(inventory)
		if e != nil || trust.Validate() != nil {
			t.Fatal("actual mutation inventory invalid")
		}
		body, _ := json.Marshal(trust)
		write("build-trust.json", map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "build-trust", "namespace": buildworkload.Namespace}, "immutable": true, "data": map[string]any{"trust.json": string(body)}})
		return
	}
	if input.Phase != "execute" && input.Phase != "preflight" {
		t.Fatal("unknown fixture phase")
	}
	for _, key := range buildworkload.DependencyKeys() {
		path, _ := dependencyPath(key)
		_, body, e := r.getBuildObject(ctx, path)
		if e != nil {
			t.Fatal("actual installed dependency unavailable", key)
		}
		d, e := BuildObjectFingerprint(body)
		if e != nil {
			t.Fatal("actual dependency fingerprint invalid", key)
		}
		p.Dependencies[key] = d
	}
	write("profile.json", p)
	cfg, command, signer := closedFixtureAuthority(t, p, input.Slot, input.SourceSHA, input.TreeSHA)
	payload, _ := json.Marshal(command)
	signed, e := signer.Seal(cfg.Outer, command.ID(), "build-merged", payload)
	if e != nil {
		t.Fatal("synthetic signing")
	}
	r, e = PlanBuild(client, cfg, p, signed, payload, input.Slot)
	if e != nil {
		t.Fatal("closed authenticated compilation")
	}
	writeRaw("build.json", r.compiled.Build)
	writeRaw("publish.json", r.compiled.Publish)
	report := map[string]any{"status": "running", "scope": "isolated component fixture", "syntheticAuthority": true, "autonomousE2E": false, "liveWorkflowAdvanced": false, "releaseAuthorized": false, "buildName": r.runName("build"), "publishName": r.runName("publish"), "plan": r.Plan()}
	write("transport-report.json", report)
	if input.Phase == "preflight" {
		if r.inspectInstalledProfile(ctx) != nil {
			t.Fatal("installed profile preflight rejected")
		}
		if r.inspectBuildSlot(ctx) != nil {
			t.Fatal("actual slot custody preflight rejected")
		}
		report["status"] = "passed"
		report["workloadExecuted"] = false
		write("transport-report.json", report)
		return
	}
	for _, phase := range []string{"build", "publish"} {
		if r.inspectInstalledProfile(ctx) != nil {
			t.Fatal("installed profile preflight rejected", phase)
		}
		if r.inspectBuildSlot(ctx) != nil {
			t.Fatal("actual slot custody preflight rejected", phase)
		}
		if phase == "publish" {
			again, e := r.observePhase(ctx, "build", r.knownRuns["build"])
			if e != nil || !again.Complete {
				t.Fatal("original builder no longer terminal")
			}
		}
		uid, e := r.postRun(ctx, phase)
		if e != nil {
			t.Fatal("original one-shot create unconfirmed", phase)
		}
		report[phase+"RunUID"] = uid
		write("transport-report.json", report)
		for {
			observation, e := r.observePhase(ctx, phase, uid)
			if e != nil {
				t.Fatal("original hierarchy observation rejected", phase)
			}
			if observation.Complete {
				report[phase+"Termination"] = observation.Termination
				if phase == "publish" {
					report["results"] = observation.Results
				}
				write("transport-report.json", report)
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("bounded original phase deadline")
			case <-time.After(2 * time.Second):
			}
		}
		t.Log("original closed phase completed", phase)
	}
	report["status"] = "passed"
	write("transport-report.json", report)
}
func closedFixtureAuthority(t *testing.T, p buildworkload.Profile, slot building.Slot, source, tree string) (building.Configuration, building.Command, authority.Identity) {
	t.Helper()
	m, e := authority.Bootstrap("os-santiago/saferus", "ai-sdlc-production")
	if e != nil {
		t.Fatal("synthetic bootstrap")
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
		case "release-manager", "build-controller", "verifier", "deployer", "relay":
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
	cfg := building.Configuration{Inner: inner, Outer: outer, ProfileSHA256: p.Digest(), IntakeConfigSHA256: strings.Repeat("d", 64), IssueMaxAgeSeconds: 60, Slots: []building.Slot{slot}}
	now := time.Now()
	c := building.Command{Version: building.Version, MergeID: "merge-" + strings.Repeat("e", 64), MergeRecordSHA256: strings.Repeat("f", 64), Repository: inner.Repository, Environment: inner.Environment, IssueNumber: 3, IssueRevision: strings.Repeat("a", 64), Generation: 1, SourceSHA: source, TreeSHA: tree, Recipe: building.Recipe, ProfileSHA256: cfg.ProfileDigest(), IntakeConfigSHA256: cfg.IntakeConfigSHA256, InnerPolicyRevision: inner.Revision(), OuterPolicyRevision: outer.Revision(), IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(building.MaxAge).UnixMilli()}
	return cfg, c, m.Identities["release-manager"]
}
func prepareClosedFixtureSlot(t *testing.T, s building.Slot) {
	t.Helper()
	root := "/var/local/ai-sdlc/build-slots/" + s.StorageIdentitySHA256
	if os.Geteuid() != 0 {
		t.Fatal("isolated custody setup requires node root")
	}
	entries, e := os.ReadDir(root)
	if e != nil || len(entries) != 0 {
		t.Fatal("fixture slot not empty")
	}
	var fs unix.Statfs_t
	if unix.Statfs(root, &fs) != nil || fs.Type != unix.TMPFS_MAGIC || uint64(fs.Blocks)*uint64(fs.Bsize) != 4<<30 || fs.Files != 32768 {
		t.Fatal("fixture mount not bounded tmpfs")
	}
	boot, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil || strings.TrimSpace(string(boot)) != s.NodeBootID {
		t.Fatal("fixture node boot changed")
	}
	marker := buildstorage.Marker{Version: "ai-sdlc.local-build-custody/v1", StorageIdentitySHA256: s.StorageIdentitySHA256, NodeBootID: s.NodeBootID, FSID: fs.Fsid.Val, CapacityBytes: 4 << 30, InodeLimit: 32768}
	raw, _ := json.Marshal(marker)
	file, e := os.OpenFile(root+"/custody.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0444)
	if e != nil {
		t.Fatal("exclusive fixture marker")
	}
	_, writeErr := file.Write(raw)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("fixture marker write")
	}
	if os.Mkdir(root+"/workspace", 0700) != nil || os.Chown(root+"/workspace", 1000, 1000) != nil || os.Chmod(root, 0555) != nil {
		t.Fatal("fixture custody permissions")
	}
}
