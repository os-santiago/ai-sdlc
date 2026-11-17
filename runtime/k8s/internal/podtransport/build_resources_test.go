package podtransport

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func buildResourceFixture(t *testing.T, handler func(*BuildResources, http.ResponseWriter, *http.Request)) (*BuildResources, *atomic.Int32, *buildworkload.Profile) {
	t.Helper()
	var resources *BuildResources
	calls := &atomic.Int32{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 32) {
			t.Error("missing explicit credential")
		}
		handler(resources, w, r)
	}))
	t.Cleanup(server.Close)
	client, e := NewClient(Config{APIServer: server.URL, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), BearerToken: strings.Repeat("x", 32)})
	if e != nil {
		t.Fatal(e)
	}
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
	slot := building.Slot{ClusterUID: uid, NamespaceUID: uid, PVCUID: uid, PVUID: uid, NodeUID: uid, NodeBootID: uid, StorageIdentitySHA256: strings.Repeat("a", 64), Namespace: buildworkload.Namespace, PVC: "build-slot-0", Node: "fixture-node", CapacityBytes: 4 << 30, InodeLimit: 32768}
	p := buildworkload.Profile{Version: "ai-sdlc.saferus-build-profile/v1", ClusterUID: uid, NamespaceUID: uid, PublisherImage: "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("b", 64), Dependencies: map[string]buildworkload.Dependency{}, DeniedTargets: []buildworkload.DeniedTarget{{Host: "10.96.0.1", Port: 443}, {Host: "10.96.0.2", Port: 443}}}
	for _, key := range buildworkload.DependencyKeys() {
		p.Dependencies[key] = buildworkload.Dependency{UID: uid, SpecSHA256: strings.Repeat("c", 64)}
	}
	cfg := building.Configuration{Inner: inner, Outer: outer, ProfileSHA256: p.Digest(), IntakeConfigSHA256: strings.Repeat("d", 64), IssueMaxAgeSeconds: 60, Slots: []building.Slot{slot}}
	now := time.Now()
	command := building.Command{Version: building.Version, MergeID: "merge-" + strings.Repeat("e", 64), MergeRecordSHA256: strings.Repeat("f", 64), Repository: inner.Repository, Environment: inner.Environment, IssueNumber: 3, IssueRevision: strings.Repeat("a", 64), Generation: 1, SourceSHA: strings.Repeat("b", 40), TreeSHA: strings.Repeat("c", 40), Recipe: building.Recipe, ProfileSHA256: cfg.ProfileDigest(), IntakeConfigSHA256: cfg.IntakeConfigSHA256, InnerPolicyRevision: inner.Revision(), OuterPolicyRevision: outer.Revision(), IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(building.MaxAge).UnixMilli()}
	raw, _ := json.Marshal(command)
	signed, e := m.Identities["release-manager"].Seal(outer, command.ID(), "build-merged", raw)
	if e != nil {
		t.Fatal(e)
	}
	resources, e = PlanBuild(client, cfg, p, signed, raw, slot)
	if e != nil {
		t.Fatal(e)
	}
	return resources, calls, &p
}
func runResponse(r *BuildResources, phase string) map[string]any {
	out := cloneObject(r.expected[phase])
	out["metadata"].(map[string]any)["uid"] = fixtureConfigUID
	out["spec"] = defaultRunSpec(out["spec"].(map[string]any))
	return out
}
func TestBuildCreateHasOnePOSTAndNoAdoptionAfterUncertainReply(t *testing.T) {
	for _, phase := range []string{"build", "publish"} {
		for _, which := range []string{"success", "uid", "script", "unknown-field", "label", "owner", "finalizer", "conflict", "redirect", "lost-response", "oversized"} {
			t.Run(phase+"/"+which, func(t *testing.T) {
				r, calls, _ := buildResourceFixture(t, func(r *BuildResources, w http.ResponseWriter, req *http.Request) {
					if req.Method != "POST" || req.URL.Path != runPath("") {
						t.Error("unexpected path/method")
					}
					var sent map[string]any
					if json.NewDecoder(req.Body).Decode(&sent) != nil {
						t.Fatal("invalid posted JSON")
					}
					expected := r.expected[phase]
					a, _ := json.Marshal(sent)
					b, _ := json.Marshal(expected)
					if string(a) != string(b) {
						t.Error("POST differs from persisted plan bytes")
					}
					obj := runResponse(r, phase)
					meta := obj["metadata"].(map[string]any)
					spec := obj["spec"].(map[string]any)
					switch which {
					case "uid":
						meta["uid"] = "bad"
					case "script":
						spec["pipelineSpec"].(map[string]any)["tasks"].([]any)[0].(map[string]any)["taskSpec"].(map[string]any)["steps"].([]any)[0].(map[string]any)["script"] = "echo injected"
					case "unknown-field":
						spec["unreviewed"] = true
					case "label":
						meta["labels"].(map[string]any)["injected"] = "yes"
					case "owner":
						meta["ownerReferences"] = []any{map[string]any{"uid": fixtureOtherUID}}
					case "finalizer":
						meta["finalizers"] = []any{"unreviewed"}
					case "conflict":
						w.WriteHeader(409)
						return
					case "redirect":
						http.Redirect(w, req, "/adopt", 307)
						return
					case "lost-response":
						conn, _, _ := w.(http.Hijacker).Hijack()
						_ = conn.Close()
						return
					case "oversized":
						w.WriteHeader(201)
						_, _ = w.Write([]byte(strings.Repeat("x", maxBuildObject+1)))
						return
					}
					writeResource(w, 201, obj)
				})
				if phase == "publish" {
					seedBuildCompletionForHTTPTest(r)
				}
				uid, e := r.postRun(context.Background(), phase)
				if which == "success" {
					if e != nil || uid != fixtureConfigUID {
						t.Fatal("valid create denied", e)
					}
				} else if e != ErrUnknownBuildCreate || uid != "" {
					t.Fatal("uncertain create not quarantined", e)
				}
				if _, second := r.postRun(context.Background(), phase); second != ErrBuildTransport {
					t.Fatal("same create intent reusable")
				}
				if calls.Load() != 1 {
					t.Fatal("create retried or adopted")
				}
			})
		}
	}
}
func TestBuildCreateExpiredBeforePOSTHasNoSideEffect(t *testing.T) {
	r, calls, _ := buildResourceFixture(t, func(_ *BuildResources, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	r.compiled.ExpiresAt = time.Now().Add(-time.Second)
	if _, e := r.postRun(context.Background(), "build"); e != ErrBuildTransport || calls.Load() != 0 {
		t.Fatal("expired authority caused POST")
	}
}

func TestBuildPlanOwnsImmutableInstalledProfile(t *testing.T) {
	r, _, original := buildResourceFixture(t, func(_ *BuildResources, _ http.ResponseWriter, _ *http.Request) {
		t.Fatal("profile copy caused API call")
	})
	expected := r.profile.Digest()
	plan := r.Plan().Digest()
	original.Dependencies[buildworkload.DependencyKeys()[0]] = buildworkload.Dependency{UID: fixtureOtherUID, SpecSHA256: strings.Repeat("0", 64)}
	original.DeniedTargets[0] = buildworkload.DeniedTarget{Host: "10.99.99.99", Port: 1234}
	if r.profile.Digest() != expected || r.Plan().Digest() != plan {
		t.Fatal("caller mutation changed sealed plan or installed profile")
	}
}

// Isolates HTTP publication exchange from the separate observer tests. Runtime
// has no API for installing caller-provided completion evidence.
func seedBuildCompletionForHTTPTest(r *BuildResources) {
	uid := "22222222-2222-4222-8222-222222222222"
	r.knownRuns["build"] = uid
	r.completed["build"] = building.Termination{Version: "ai-sdlc.build-termination/v1", Phase: "build", RequestSHA256: r.compiled.Plan.BuildRequestSHA256, PipelineRunUID: uid, TaskRunUID: "33333333-3333-4333-8333-333333333333", PodUID: "44444444-4444-4444-8444-444444444444", NodeUID: r.slot.NodeUID, NodeBootID: r.slot.NodeBootID, SlotDigest: r.slot.Digest(), PodSpecSHA256: strings.Repeat("a", 64), ObservedAt: time.Now().UnixMilli(), Containers: []building.ContainerExit{{Name: "prepare", Kind: "init", ImageDigest: "sha256:" + strings.Repeat("a", 64), ContainerID: "containerd://" + strings.Repeat("a", 64), StartedAt: r.command.IssuedAt, FinishedAt: r.command.IssuedAt}, {Name: "step-verify", Kind: "normal", ImageDigest: "sha256:" + strings.Repeat("b", 64), ContainerID: "containerd://" + strings.Repeat("b", 64), StartedAt: r.command.IssuedAt, FinishedAt: r.command.IssuedAt}}}
}
func TestPublicationRequiresFreshOriginalWriterTermination(t *testing.T) {
	for _, which := range []string{"missing", "stale", "wrong-original", "failed-exit"} {
		t.Run(which, func(t *testing.T) {
			r, calls, _ := buildResourceFixture(t, func(_ *BuildResources, _ http.ResponseWriter, _ *http.Request) {
				t.Fatal("unproven writer caused publication")
			})
			if which != "missing" {
				seedBuildCompletionForHTTPTest(r)
				proof := r.completed["build"]
				switch which {
				case "stale":
					proof.ObservedAt = time.Now().Add(-time.Minute).UnixMilli()
				case "wrong-original":
					r.knownRuns["build"] = fixtureOtherUID
				case "failed-exit":
					proof.Containers[1].ExitCode = 1
				}
				r.completed["build"] = proof
			}
			if _, e := r.postRun(context.Background(), "publish"); e != ErrBuildTransport || calls.Load() != 0 {
				t.Fatal("publication accepted without fresh original termination")
			}
		})
	}
}
