package podtransport

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	core "k8s.io/api/core/v1"
	network "k8s.io/api/networking/v1"
	node "k8s.io/api/node/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type browserGateFixture struct {
	sent, ack, record int
	mode              string
	until             time.Time
	parent            context.Context
	originalUID       string
	onACK             func()
	onBefore          func()
}

func (g *browserGateFixture) CleanupDeadline() time.Time      { return g.until }
func (g *browserGateFixture) CleanupContext() context.Context { return g.parent }
func (g *browserGateFixture) BeforeCreate(context.Context, string) error {
	g.sent++
	if g.onBefore != nil {
		g.onBefore()
	}
	if g.mode == "gate-denied" {
		return browsercheck.ErrBrowser
	}
	return nil
}
func (g *browserGateFixture) AcknowledgePod(_ context.Context, _ string, uid string) error {
	g.originalUID = uid
	if g.onACK != nil {
		g.onACK()
	}
	g.ack++
	if g.mode == "ack-denied" {
		return browsercheck.ErrBrowser
	}
	return nil
}
func (g *browserGateFixture) RecordEvidence(context.Context, browsercheck.Evidence) error {
	g.record++
	if g.mode == "record-denied" {
		return browsercheck.ErrBrowser
	}
	return nil
}

type browserContainmentFixture struct{ checks, records int }

func (g *browserContainmentFixture) CheckCleanup(ctx context.Context) error {
	g.checks++
	return ctx.Err()
}
func (g *browserContainmentFixture) RecordCleanup(ctx context.Context) error {
	g.records++
	return ctx.Err()
}

// A synthetic API checks adversarial identity/cleanup transport behavior. The
// actual Firefox/runtime/network semantics are tested separately in kind.
func TestBrowserTransportOriginalIdentityAndFailClosedCleanup(t *testing.T) {
	for _, mode := range []string{"success", "gate-denied", "ack-denied", "record-denied", "extra-policy", "replaced-namespace", "wrong-image", "wrong-result", "lost-create-ack", "replacement-pod", "wrong-spec", "nonzero-exit", "cleanup-claim-cancel", "cleanup-claim-expiry", "cleanup-smoke-expiry", "foreign-node", "cordoned-node", "scope-during-gate"} {
		t.Run(mode, func(t *testing.T) {
			parent, cancelParent := context.WithCancel(context.Background())
			defer cancelParent()
			f := newConsoleFixture(t)
			f.s.Spec.ClusterIP = "10.96.0.10"
			f.target.ServiceSpecSHA256, _ = ConsoleServiceDigest(f.s.Spec)
			profile, request, _, _, _ := browserFixture(t)
			profile.ClusterUID = f.target.ClusterUID
			profile.NamespaceUID = consoleUID(21)
			profile.RuntimeClassUID = consoleUID(22)
			profile.CAConfigMapUID = consoleUID(23)
			profile.DenyPolicyUID = consoleUID(24)
			profile.EgressPolicyUID = consoleUID(25)
			profile.ConsoleIngressPolicyUID = consoleUID(26)
			profile.CASHA256 = authority.Digest([]byte(f.target.TLSCA))
			var pod *core.Pod
			var result browsercheck.Result
			creates, deletes := 0, 0
			var scopeChanged atomic.Bool
			base := "/api/v1/namespaces/" + browsercheck.Namespace
			policiesBase := "/apis/networking.k8s.io/v1/namespaces/"
			api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var obj any
				switch {
				case r.URL.Path == "/api/v1/nodes/"+profile.NodeName:
					obj = core.Node{ObjectMeta: meta.ObjectMeta{Name: profile.NodeName, UID: types.UID(profile.NodeUID)}, Status: core.NodeStatus{Conditions: []core.NodeCondition{{Type: core.NodeReady, Status: core.ConditionTrue}}}}
					if mode == "foreign-node" {
						n := obj.(core.Node)
						n.UID = types.UID(consoleUID(98))
						obj = n
					}
					if mode == "cordoned-node" {
						n := obj.(core.Node)
						n.Spec.Unschedulable = true
						obj = n
					}
				case r.URL.Path == base:
					id := profile.NamespaceUID
					if mode == "replaced-namespace" || scopeChanged.Load() {
						id = consoleUID(99)
					}
					obj = core.Namespace{ObjectMeta: meta.ObjectMeta{Name: browsercheck.Namespace, UID: types.UID(id), Labels: map[string]string{"pod-security.kubernetes.io/enforce": "restricted", "kubernetes.io/metadata.name": browsercheck.Namespace}}}
				case r.URL.Path == "/apis/node.k8s.io/v1/runtimeclasses/"+browsercheck.RuntimeClass:
					obj = node.RuntimeClass{ObjectMeta: meta.ObjectMeta{Name: browsercheck.RuntimeClass, UID: types.UID(profile.RuntimeClassUID), Annotations: map[string]string{"ai-sdlc.io/cluster-uid": profile.ClusterUID, "ai-sdlc.io/configuration-sha256": profile.RuntimeConfigurationSHA256}}, Handler: browsercheck.RuntimeClass}
				case r.URL.Path == base+"/configmaps/"+browserCA:
					obj = core.ConfigMap{ObjectMeta: meta.ObjectMeta{Name: browserCA, Namespace: browsercheck.Namespace, UID: types.UID(profile.CAConfigMapUID)}, Immutable: consolePtr(true), Data: map[string]string{"ca.crt": f.target.TLSCA}}
				case r.URL.Path == policiesBase+browsercheck.Namespace+"/networkpolicies":
					deny, egress, _ := browserPolicies()
					items := []network.NetworkPolicy{{ObjectMeta: meta.ObjectMeta{Name: "deny-all", Namespace: browsercheck.Namespace, UID: types.UID(profile.DenyPolicyUID)}, Spec: deny}, {ObjectMeta: meta.ObjectMeta{Name: "console-egress", Namespace: browsercheck.Namespace, UID: types.UID(profile.EgressPolicyUID)}, Spec: egress}}
					if mode == "extra-policy" {
						items = append(items, network.NetworkPolicy{})
					}
					obj = network.NetworkPolicyList{Items: items}
				case r.URL.Path == policiesBase+"ai-sdlc-production/networkpolicies/ai-sdlc-browser-console":
					_, _, ingress := browserPolicies()
					obj = network.NetworkPolicy{ObjectMeta: meta.ObjectMeta{Name: "ai-sdlc-browser-console", Namespace: "ai-sdlc-production", UID: types.UID(profile.ConsoleIngressPolicyUID)}, Spec: ingress}
				case r.URL.Path == base+"/pods" && r.Method == http.MethodPost:
					creates++
					pod = &core.Pod{}
					if json.NewDecoder(r.Body).Decode(pod) != nil {
						w.WriteHeader(400)
						return
					}
					pod.UID = types.UID(consoleUID(50))
					applyDefaults(&pod.Spec)
					pod.Spec.DNSPolicy = core.DNSNone
					pod.Spec.NodeName = "fixture-node"
					now := time.Now()
					result = browsercheck.Result{Version: browsercheck.ResultVersion, Status: "passed", RequestID: request.RequestID, RequestSHA256: request.Digest(), TargetSHA256: request.TargetSHA256, SnapshotSHA256: request.SnapshotSHA256, ImageDigest: request.ImageDigest, Expectation: request.Expectation, StartedAt: now.UnixMilli(), FinishedAt: now.UnixMilli(), Observations: []browsercheck.Observation{{Viewport: "desktop", ResponseSHA256: strings.Repeat("a", 64)}, {Viewport: "mobile", ResponseSHA256: strings.Repeat("b", 64)}}, NormalTLS: true, NetworkScopePreserved: true}
					identity := "containerd://" + strings.Repeat("f", 64)
					pod.Status = core.PodStatus{Phase: core.PodSucceeded, ContainerStatuses: []core.ContainerStatus{{Name: "browser", ImageID: "quay.io/os-santiago/ai-sdlc@" + profile.RuntimeDigest, ContainerID: identity, State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ContainerID: identity, StartedAt: meta.NewTime(now), FinishedAt: meta.NewTime(now)}}}}}
					if mode == "wrong-image" {
						pod.Status.ContainerStatuses[0].ImageID = "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("e", 64)
					}
					if mode == "wrong-result" {
						result.RequestID = "browser-" + strings.Repeat("c", 64)
					}
					if mode == "wrong-spec" {
						pod.Spec.Containers[0].Command = []string{"sh"}
					}
					if mode == "nonzero-exit" {
						pod.Status.ContainerStatuses[0].State.Terminated.ExitCode = 1
					}
					if mode == "lost-create-ack" {
						w.WriteHeader(503)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(201)
					json.NewEncoder(w).Encode(pod)
					return
				case r.URL.Path == base+"/pods":
					items := []core.Pod{}
					if pod != nil {
						items = append(items, *pod)
					}
					obj = core.PodList{Items: items}
				case strings.HasPrefix(r.URL.Path, base+"/pods/"):
					if pod == nil {
						w.WriteHeader(404)
						return
					}
					if strings.HasSuffix(r.URL.Path, "/log") {
						if mode == "cleanup-claim-cancel" {
							cancelParent()
						}
						if mode == "cleanup-claim-expiry" || mode == "cleanup-smoke-expiry" {
							time.Sleep(800 * time.Millisecond)
						}
						obj = result
						break
					}
					if mode == "replacement-pod" {
						pod.UID = types.UID(consoleUID(51))
					}
					if r.Method == http.MethodDelete {
						var options meta.DeleteOptions
						if json.NewDecoder(r.Body).Decode(&options) != nil || options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != pod.UID {
							w.WriteHeader(409)
							return
						}
						deletes++
						pod = nil
						obj = meta.Status{Status: "Success"}
					} else {
						obj = pod
					}
				default:
					f.serve(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(obj)
			}))
			defer api.Close()
			client, e := NewClient(Config{APIServer: api.URL, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw}), BearerToken: strings.Repeat("x", 40)})
			if e != nil {
				t.Fatal(e)
			}
			adapter, e := NewConsoleDeployment(client, f.target)
			if e != nil {
				t.Fatal(e)
			}
			snapshot, e := adapter.Observe(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			route, ready, e := adapter.ObserveRollout(context.Background(), snapshot)
			if e != nil || !ready {
				t.Fatal("fixture rollout", e)
			}
			raw, _ := json.Marshal(snapshot)
			request.TargetSHA256 = f.target.Digest()
			request.SnapshotSHA256 = authority.Digest(raw)
			request.ImageDigest = f.target.BaselineRuntimeDigest
			request.IssuedAt = time.Now().UnixMilli()
			request.ExpiresAt = time.Now().Add(25 * time.Second).UnixMilli()
			request.Expectation = profile.BaselineExpectation
			if mode == "cleanup-claim-expiry" || mode == "cleanup-smoke-expiry" {
				request.ExpiresAt = time.Now().Add(600 * time.Millisecond).UnixMilli()
			}
			b, e := NewBrowserResources(adapter, profile, request, route)
			if e != nil {
				t.Fatal(e)
			}
			gate := &browserGateFixture{mode: mode, until: time.UnixMilli(request.ExpiresAt), parent: parent}
			if mode == "scope-during-gate" {
				gate.onBefore = func() { scopeChanged.Store(true) }
			}
			if mode == "cleanup-smoke-expiry" {
				gate.until = time.Now().Add(5 * time.Second)
			}
			_, evidence, e := b.Verify(context.Background(), snapshot, route, gate)
			if mode == "success" {
				if e != nil || evidence.Validate(profile, time.Now()) != nil || creates != 1 || deletes != 1 || gate.sent != 1 || gate.ack != 1 || gate.record != 1 {
					t.Fatalf("success lifecycle: err=%v create=%d delete=%d gate=%+v", e, creates, deletes, gate)
				}
			} else {
				if e == nil {
					t.Fatal("unsafe result accepted")
				}
				if mode != "record-denied" && gate.record != 0 {
					t.Fatal("unsafe observation reached evidence writer")
				}
				if mode == "gate-denied" || mode == "extra-policy" || mode == "replaced-namespace" || mode == "foreign-node" || mode == "cordoned-node" || mode == "scope-during-gate" {
					if creates != 0 {
						t.Fatal("created after failed precondition")
					}
				} else if creates != 1 {
					t.Fatal("creation replay or missing negative control")
				}
				if mode == "lost-create-ack" || mode == "replacement-pod" || mode == "cleanup-claim-cancel" || mode == "cleanup-claim-expiry" {
					if deletes != 0 {
						t.Fatal("adopted unacknowledged/replacement Pod")
					}
				} else if creates == 1 && deletes != 1 {
					t.Fatal("original acknowledged Pod not cleaned")
				}
			}
			if mode == "cleanup-claim-expiry" || mode == "replacement-pod" || mode == "lost-create-ack" {
				cleaner, err := NewBrowserCleanupClient(client)
				if err != nil {
					t.Fatal(err)
				}
				fresh, stop := context.WithTimeout(context.Background(), 2*time.Second)
				defer stop()
				containment := &browserContainmentFixture{}
				originalUID := consoleUID(50)
				if mode == "lost-create-ack" {
					originalUID = ""
				}
				err = cleaner.CleanupBrowser(fresh, profile, request, route, originalUID, containment)
				if mode == "cleanup-claim-expiry" {
					if err != nil || deletes != 1 || containment.records != 1 || gate.record != 0 {
						t.Fatal("bounded containment failed or invented health", err)
					}
				} else if err == nil || deletes != 0 || containment.records != 0 {
					t.Fatal("containment adopted unknown/replacement Pod")
				}
			}
		})
	}
}
