package podtransport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"io"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func consoleUID(n int) string  { return fmt.Sprintf("%08d-1111-1111-1111-111111111111", n) }
func consolePtr[T any](v T) *T { return &v }
func consoleCertificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "disposable-console-fixture"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, e := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "saferus-console"}, DNSNames: []string{"saferus-console"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	cert, e := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	return tls.Certificate{Certificate: [][]byte{cert, der}, PrivateKey: key}, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

type consoleFixture struct {
	t       *testing.T
	mu      sync.Mutex
	target  releasing.Target
	d       apps.Deployment
	s       core.Service
	rs      apps.ReplicaSet
	p       core.Pod
	ep      discovery.EndpointSlice
	api     *httptest.Server
	client  *Client
	adapter *ConsoleDeployment
	patches int
	drop    bool
	tamper  func(string, any) any
}

func newConsoleFixture(t *testing.T) *consoleFixture {
	t.Helper()
	_, ca := consoleCertificate(t)
	f := &consoleFixture{t: t}
	f.d = apps.Deployment{TypeMeta: meta.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: meta.ObjectMeta{Name: consoleName, Namespace: consoleNS, UID: types.UID(consoleUID(3)), ResourceVersion: "100", Generation: 1}, Spec: apps.DeploymentSpec{Replicas: consolePtr(int32(1)), Selector: &meta.LabelSelector{MatchLabels: map[string]string{"app": consoleName}}, Template: core.PodTemplateSpec{ObjectMeta: meta.ObjectMeta{Labels: map[string]string{"app": consoleName}}, Spec: core.PodSpec{ServiceAccountName: "saferus-client", AutomountServiceAccountToken: consolePtr(false), SecurityContext: &core.PodSecurityContext{RunAsNonRoot: consolePtr(true), RunAsUser: consolePtr(int64(1000)), SeccompProfile: &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault}}, ImagePullSecrets: []core.LocalObjectReference{{Name: "ai-sdlc-release-reader"}}, Containers: []core.Container{{Name: "console", Image: "localhost/saferus-console@sha256:" + strings.Repeat("7", 64), SecurityContext: &core.SecurityContext{ReadOnlyRootFilesystem: consolePtr(true), AllowPrivilegeEscalation: consolePtr(false), Capabilities: &core.Capabilities{Drop: []core.Capability{"ALL"}}}}}}}}}
	f.s = core.Service{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: meta.ObjectMeta{Name: consoleName, Namespace: consoleNS, UID: types.UID(consoleUID(4))}, Spec: core.ServiceSpec{Type: core.ServiceTypeClusterIP, ClusterIP: "127.0.0.1", Ports: []core.ServicePort{{Name: "https", Port: 8443, TargetPort: intstr.FromString("https"), Protocol: core.ProtocolTCP}}, Selector: map[string]string{"app": consoleName}}}
	dh, e := ConsoleSpecDigest(f.d.Spec)
	if e != nil {
		t.Fatal(e)
	}
	sh, e := ConsoleServiceDigest(f.s.Spec)
	if e != nil {
		t.Fatal(e)
	}
	f.target = releasing.Target{Version: releasing.TargetVersion, Scope: "kind-test-production", Repository: "os-santiago/saferus", Environment: consoleNS, ClusterUID: consoleUID(1), NamespaceUID: consoleUID(2), Namespace: consoleNS, Deployment: consoleName, DeploymentUID: consoleUID(3), Container: "console", DeploymentSpecSHA256: dh, Service: consoleName, ServiceUID: consoleUID(4), ServiceSpecSHA256: sh, PullSecret: "ai-sdlc-release-reader", PullSecretUID: consoleUID(5), TLSSecretUID: consoleUID(6), TLSCA: ca, TLSHostname: "saferus-console", BaselineImage: f.d.Spec.Template.Spec.Containers[0].Image, BaselineRuntimeDigest: "sha256:" + strings.Repeat("8", 64), RolloutSeconds: 300, SmokeSeconds: 30, ApprovedAt: time.Now().Add(-time.Minute).UnixMilli(), ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	f.ready()
	f.api = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.api.Close)
	apiCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.api.Certificate().Raw})
	f.client, e = NewClient(Config{APIServer: f.api.URL, CA: apiCA, BearerToken: strings.Repeat("x", 40)})
	if e != nil {
		t.Fatal(e)
	}
	f.adapter, e = NewConsoleDeployment(f.client, f.target)
	if e != nil {
		t.Fatal(e)
	}
	return f
}
func (f *consoleFixture) ready() {
	f.d.Status = apps.DeploymentStatus{ObservedGeneration: f.d.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	f.rs = apps.ReplicaSet{TypeMeta: meta.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSet"}, ObjectMeta: meta.ObjectMeta{Name: "saferus-console-current", Namespace: consoleNS, UID: types.UID(consoleUID(7)), Generation: 1, OwnerReferences: []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: consoleName, UID: f.d.UID, Controller: consolePtr(true)}}}, Spec: apps.ReplicaSetSpec{Replicas: consolePtr(int32(1)), Template: *f.d.Spec.Template.DeepCopy()}, Status: apps.ReplicaSetStatus{ObservedGeneration: 1, Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}}
	f.rs.Spec.Template.Labels["pod-template-hash"] = "fixture"
	spec := *f.d.Spec.Template.Spec.DeepCopy()
	spec.NodeName = "fixture-control-plane"
	spec.EnableServiceLinks = consolePtr(true)
	spec.Priority = consolePtr(int32(0))
	spec.PreemptionPolicy = consolePtr(core.PreemptLowerPriority)
	spec.DeprecatedServiceAccount = spec.ServiceAccountName
	v := int64(300)
	spec.Tolerations = []core.Toleration{{Key: "node.kubernetes.io/not-ready", Operator: core.TolerationOpExists, Effect: core.TaintEffectNoExecute, TolerationSeconds: &v}, {Key: "node.kubernetes.io/unreachable", Operator: core.TolerationOpExists, Effect: core.TaintEffectNoExecute, TolerationSeconds: &v}}
	image := spec.Containers[0].Image
	imageID := image
	if image == f.target.BaselineImage {
		imageID = "docker.io/library/import-fixture@" + f.target.BaselineRuntimeDigest
	}
	statusImage := image
	if image == f.target.BaselineImage {
		statusImage = "localhost/saferus-console:fixture-import"
	}
	f.p = core.Pod{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: meta.ObjectMeta{Name: "saferus-console-current-pod", Namespace: consoleNS, UID: types.UID(consoleUID(8)), Labels: map[string]string{"app": consoleName, "pod-template-hash": "fixture"}, OwnerReferences: []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: f.rs.Name, UID: f.rs.UID, Controller: consolePtr(true)}}}, Spec: spec, Status: core.PodStatus{Phase: core.PodRunning, PodIP: "10.244.0.10", Conditions: []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}, ContainerStatuses: []core.ContainerStatus{{Name: "console", Image: statusImage, ImageID: imageID, ContainerID: "containerd://" + strings.Repeat("9", 64), Ready: true, State: core.ContainerState{Running: &core.ContainerStateRunning{}}}}}}
	f.ep = discovery.EndpointSlice{TypeMeta: meta.TypeMeta{APIVersion: "discovery.k8s.io/v1", Kind: "EndpointSlice"}, ObjectMeta: meta.ObjectMeta{Name: "saferus-console-fixture", Namespace: consoleNS, UID: types.UID(consoleUID(9)), OwnerReferences: []meta.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: consoleName, UID: f.s.UID, Controller: consolePtr(true)}}}, AddressType: discovery.AddressTypeIPv4, Ports: []discovery.EndpointPort{{Name: consolePtr("https"), Port: consolePtr(int32(8443)), Protocol: consolePtr(core.ProtocolTCP)}}, Endpoints: []discovery.Endpoint{{Addresses: []string{f.p.Status.PodIP}, Conditions: discovery.EndpointConditions{Ready: consolePtr(true)}, NodeName: consolePtr(spec.NodeName), TargetRef: &core.ObjectReference{Kind: "Pod", Namespace: consoleNS, Name: f.p.Name, UID: f.p.UID}}}}
}
func (f *consoleFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 40) {
		w.WriteHeader(403)
		return
	}
	var obj any
	switch r.URL.Path {
	case "/api/v1/namespaces/kube-system", "/api/v1/namespaces/" + consoleNS:
		name, uid := "kube-system", f.target.ClusterUID
		if strings.HasSuffix(r.URL.Path, consoleNS) {
			name, uid = consoleNS, f.target.NamespaceUID
		}
		obj = core.Namespace{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: meta.ObjectMeta{Name: name, UID: types.UID(uid)}}
	case "/api/v1/namespaces/" + consoleNS + "/secrets/ai-sdlc-release-reader":
		obj = core.Secret{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: meta.ObjectMeta{Name: f.target.PullSecret, Namespace: consoleNS, UID: types.UID(f.target.PullSecretUID)}, Immutable: consolePtr(true), Type: core.SecretTypeDockerConfigJson}
	case "/api/v1/namespaces/" + consoleNS + "/secrets/saferus-console-tls":
		obj = core.Secret{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: meta.ObjectMeta{Name: "saferus-console-tls", Namespace: consoleNS, UID: types.UID(f.target.TLSSecretUID)}, Type: core.SecretTypeTLS}
	case "/api/v1/namespaces/" + consoleNS + "/services/" + consoleName:
		obj = f.s
	case consolePath:
		if r.Method == http.MethodPatch {
			f.patches++
			raw, _ := io.ReadAll(io.LimitReader(r.Body, 8193))
			var patch []struct {
				Op, Path string
				Value    json.RawMessage
			}
			if r.Header.Get("Content-Type") != "application/json-patch+json" || json.Unmarshal(raw, &patch) != nil || len(patch) != 6 {
				w.WriteHeader(400)
				return
			}
			snapshot := ConsoleSnapshot{DeploymentUID: string(f.d.UID), ResourceVersion: f.d.ResourceVersion, Generation: f.d.Generation, Image: f.d.Spec.Template.Spec.Containers[0].Image, TargetSHA256: f.target.Digest(), ObservedAt: time.Now().UnixMilli()}
			var image string
			if json.Unmarshal(patch[5].Value, &image) != nil {
				w.WriteHeader(400)
				return
			}
			expected, e := ConsolePatch(f.target, snapshot, image)
			if e != nil || !reflect.DeepEqual(json.RawMessage(raw), json.RawMessage(expected)) {
				w.WriteHeader(409)
				return
			}
			f.d.Spec.Template.Spec.Containers[0].Image = image
			f.d.Generation++
			f.d.ResourceVersion = fmt.Sprint(100 + f.d.Generation)
			f.ready()
			if f.drop {
				h, ok := w.(http.Hijacker)
				if ok {
					conn, _, _ := h.Hijack()
					_ = conn.Close()
				}
				return
			}
		}
		obj = f.d
	case "/apis/apps/v1/namespaces/" + consoleNS + "/replicasets":
		obj = apps.ReplicaSetList{Items: []apps.ReplicaSet{f.rs}}
	case "/api/v1/namespaces/" + consoleNS + "/pods":
		obj = core.PodList{Items: []core.Pod{f.p}}
	case "/apis/discovery.k8s.io/v1/namespaces/" + consoleNS + "/endpointslices":
		obj = discovery.EndpointSliceList{Items: []discovery.EndpointSlice{f.ep}}
	default:
		w.WriteHeader(404)
		return
	}
	if f.tamper != nil {
		obj = f.tamper(r.URL.Path, obj)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(obj)
}

type consoleGate struct {
	call func(context.Context, ConsoleSnapshot, string, bool) error
}

func (g consoleGate) CheckConsoleEffect(ctx context.Context, s ConsoleSnapshot, d string, b bool) error {
	return g.call(ctx, s, d, b)
}
func (f *consoleFixture) plan() releasing.Plan {
	now := time.Now()
	return releasing.Plan{Version: releasing.PlanVersion, TargetSHA256: f.target.Digest(), PreviousImage: f.target.BaselineImage, Image: building.Registry + "@sha256:" + strings.Repeat("a", 64), CompiledAt: now.UnixMilli(), ExpiresAt: now.Add(15 * time.Minute).UnixMilli()}
}
func allowConsoleGate() consoleGate {
	return consoleGate{func(context.Context, ConsoleSnapshot, string, bool) error { return nil }}
}
func TestConsoleConditionalImageOnlyApplyAndOwnedRollback(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()
	before, e := f.adapter.Observe(ctx)
	if e != nil {
		t.Fatal(e)
	}
	effect := &ConsoleEffects{Deployment: f.adapter, Gate: allowConsoleGate()}
	receipt, e := effect.Apply(ctx, f.plan(), before)
	if e != nil {
		t.Fatal(e)
	}
	if receipt.After.Generation != 2 || receipt.Before != before || f.patches != 1 {
		t.Fatal("patch receipt")
	}
	if _, e = effect.Apply(ctx, f.plan(), before); e == nil {
		t.Fatal("instance replay")
	}
	rollout, e := f.adapter.WaitReady(ctx, receipt.After)
	if e != nil || rollout.Image != f.plan().Image {
		t.Fatal("rollout", e)
	}
	// Status updates may change RV; original generation/candidate must remain.
	f.d.ResourceVersion = "200"
	current, e := f.adapter.Observe(ctx)
	if e != nil {
		t.Fatal(e)
	}
	rolled, e := effect.Rollback(ctx, receipt, current)
	if e != nil || !rolled.Rollback || rolled.After.Image != f.target.BaselineImage {
		t.Fatal("rollback", e)
	}
	if _, e = f.adapter.WaitReady(ctx, rolled.After); e != nil {
		t.Fatal("approved baseline runtime descriptor", e)
	}
	if f.patches != 2 {
		t.Fatal("unexpected patch count")
	}
}
func TestConsoleDeniedGateAndAmbiguousPatchCannotReplay(t *testing.T) {
	for _, name := range []string{"gate", "lost-ack", "resource-version", "future-rollout"} {
		t.Run(name, func(t *testing.T) {
			f := newConsoleFixture(t)
			ctx := context.Background()
			before, e := f.adapter.Observe(ctx)
			if e != nil {
				t.Fatal(e)
			}
			gate := allowConsoleGate()
			if name == "gate" {
				gate = consoleGate{func(context.Context, ConsoleSnapshot, string, bool) error { return errors.New("revoked") }}
			}
			if name == "lost-ack" {
				f.drop = true
			}
			if name == "resource-version" {
				f.d.ResourceVersion = "300"
			}
			effect := &ConsoleEffects{Deployment: f.adapter, Gate: gate}
			receipt, e := effect.Apply(ctx, f.plan(), before)
			if name == "future-rollout" {
				if e != nil {
					t.Fatal(e)
				}
				f.d.Generation++
				f.d.ResourceVersion = "400"
				current, e := f.adapter.Observe(ctx)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = effect.Rollback(ctx, receipt, current); e == nil {
					t.Fatal("overwrote later rollout")
				}
				if f.patches != 1 {
					t.Fatal("rollback request sent")
				}
				return
			}
			if e == nil || receipt.After.DeploymentUID != "" {
				t.Fatal("invalid/uncertain receipt")
			}
			if (name == "lost-ack") != errors.Is(e, ErrConsoleUncertain) {
				t.Fatal("effect certainty classification", e)
			}
			if _, e = effect.Apply(ctx, f.plan(), before); e == nil {
				t.Fatal("effect replay")
			}
			expected := 0
			if name == "lost-ack" {
				expected = 1
			}
			if f.patches != expected {
				t.Fatal("request count", f.patches)
			}
		})
	}
}
func TestConsoleRolloutRejectsDriftAndForeignRouting(t *testing.T) {
	for _, name := range []string{"deployment-uid", "spec", "service", "replicaset-owner", "pod-owner", "pod-admission", "runtime-digest", "endpoint-owner", "endpoint-pod", "endpoint-address", "secret-uid", "target-expired"} {
		t.Run(name, func(t *testing.T) {
			f := newConsoleFixture(t)
			ctx := context.Background()
			snapshot, e := f.adapter.Observe(ctx)
			if e != nil {
				t.Fatal(e)
			}
			f.mu.Lock()
			switch name {
			case "deployment-uid":
				f.d.UID = types.UID(consoleUID(30))
			case "spec":
				f.d.Spec.Template.Spec.Containers[0].Args = []string{"unapproved"}
			case "service":
				f.s.Spec.ClusterIP = "127.0.0.2"
			case "replicaset-owner":
				f.rs.OwnerReferences[0].UID = types.UID(consoleUID(30))
			case "pod-owner":
				f.p.OwnerReferences[0].UID = types.UID(consoleUID(30))
			case "pod-admission":
				f.p.Spec.Containers[0].Args = []string{"unapproved"}
			case "runtime-digest":
				f.p.Status.ContainerStatuses[0].ImageID = "attacker@sha256:" + strings.Repeat("f", 64)
			case "endpoint-owner":
				f.ep.OwnerReferences[0].UID = types.UID(consoleUID(30))
			case "endpoint-pod":
				f.ep.Endpoints[0].TargetRef.UID = types.UID(consoleUID(30))
			case "endpoint-address":
				f.ep.Endpoints[0].Addresses = []string{"203.0.113.9"}
			case "secret-uid":
				f.tamper = func(path string, obj any) any {
					if strings.HasSuffix(path, "/secrets/ai-sdlc-release-reader") {
						s := obj.(core.Secret)
						s.UID = types.UID(consoleUID(30))
						return s
					}
					return obj
				}
			case "target-expired":
				f.adapter.target.ExpiresAt = time.Now().UnixMilli()
			}
			f.mu.Unlock()
			if _, ready, e := f.adapter.ObserveRollout(ctx, snapshot); ready || e == nil && name != "endpoint-pod" {
				t.Fatal("drift or foreign route accepted")
			}
			if f.patches != 0 {
				t.Fatal("read-only observation mutated API")
			}
		})
	}
}
func TestConsoleWaitReadinessHonorsCallerDeadline(t *testing.T) {
	f := newConsoleFixture(t)
	snapshot, e := f.adapter.Observe(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	f.d.Status.ReadyReplicas = 0
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, e = f.adapter.WaitReady(ctx, snapshot); e == nil || time.Since(start) > time.Second {
		t.Fatal("readiness wait not bounded", e)
	}
}
func TestConsoleTLSServiceSmokeAndPostObservationFence(t *testing.T) {
	for _, name := range []string{"valid", "redirect", "busy", "unknown-field", "alias", "invalid-ttl", "not-revoked", "wrong-ca", "endpoint-change"} {
		t.Run(name, func(t *testing.T) {
			f := newConsoleFixture(t)
			cert, ca := consoleCertificate(t)
			f.target.TLSCA = ca
			if name == "wrong-ca" {
				_, f.target.TLSCA = consoleCertificate(t)
			}
			var e error
			f.adapter, e = NewConsoleDeployment(f.client, f.target)
			if e != nil {
				t.Fatal(e)
			}
			listener, e := net.Listen("tcp", "127.0.0.1:8443")
			if e != nil {
				t.Fatal(e)
			}
			server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host != "saferus-console:8443" || r.TLS == nil || r.TLS.ServerName != "saferus-console" {
					t.Error("fixed hostname not used")
					w.WriteHeader(400)
					return
				}
				w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'")
				w.Header().Set("X-Content-Type-Options", "nosniff")
				w.Header().Set("Referrer-Policy", "no-referrer")
				w.Header().Set("Cache-Control", "no-store")
				if name == "redirect" {
					w.Header().Set("Location", "https://untrusted.invalid/")
					w.WriteHeader(302)
					return
				}
				switch r.URL.Path {
				case "/health/live":
					w.WriteHeader(204)
				case "/":
					_, _ = io.WriteString(w, "<title>Saferus · Workload vault</title><button>Run verification</button>")
				case "/api/demo":
					raw, _ := io.ReadAll(io.LimitReader(r.Body, 17))
					if r.Method != "POST" || string(raw) != "{}" || r.Header.Get("Origin") != "https://saferus-console:8443" || r.Header.Get("Content-Type") != "application/json" {
						t.Error("unbounded or changed synthetic smoke")
						w.WriteHeader(400)
						return
					}
					if name == "busy" {
						w.WriteHeader(429)
						return
					}
					body := `{"verified":true,"algorithm":"AES-256-GCM","tokenTTLSeconds":60,"tokenRevoked":true,"agentless":true}`
					switch name {
					case "unknown-field":
						body = strings.TrimSuffix(body, "}") + `,"url":"https://untrusted.invalid"}`
					case "alias":
						body = strings.Replace(body, `"verified"`, `"Verified"`, 1)
					case "invalid-ttl":
						body = strings.Replace(body, `:60`, `:61`, 1)
					case "not-revoked":
						body = strings.Replace(body, `"tokenRevoked":true`, `"tokenRevoked":false`, 1)
					case "endpoint-change":
						f.mu.Lock()
						f.ep.Endpoints[0].TargetRef.UID = types.UID(consoleUID(30))
						f.mu.Unlock()
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, body)
				default:
					w.WriteHeader(404)
				}
			})}
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = server.Serve(tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}))
			}()
			t.Cleanup(func() { _ = server.Close(); _ = listener.Close(); <-done })
			snapshot, e := f.adapter.Observe(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			proof, e := f.adapter.VerifyHealth(context.Background(), snapshot)
			if name == "valid" {
				if e != nil || proof.Version != "ai-sdlc.saferus-health/v1" || len(proof.ResponseSHA256) != 3 || proof.Rollout.PodUID != string(f.p.UID) || proof.Rollout.TargetSHA256 != f.target.Digest() {
					t.Fatal("actual TLS smoke", e)
				}
				if !hex64.MatchString(proof.Digest()) || proof.ResponseSHA256[0] != authority.Digest(nil) {
					t.Fatal("bounded health evidence")
				}
			} else if e == nil || proof.Version != "" {
				t.Fatal("invalid health accepted", name)
			}
			if f.patches != 0 {
				t.Fatal("smoke mutated API")
			}
		})
	}
}
func TestConsoleAdvanceRequiresReservedContainmentBudget(t *testing.T) {
	f := newConsoleFixture(t)
	before, e := f.adapter.Observe(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	p := f.plan()
	p.ExpiresAt = time.Now().Add(releasing.MinimumEffectBudget - time.Second).UnixMilli()
	effect := &ConsoleEffects{Deployment: f.adapter, Gate: allowConsoleGate()}
	if _, e = effect.Apply(context.Background(), p, before); e == nil || f.patches != 0 {
		t.Fatal("advance consumed containment budget")
	}
}

func TestConsoleApprovedCustomTolerationsRetainAdmissionDefaults(t *testing.T) {
	for _, kind := range []string{"custom", "explicit", "wildcard"} {
		t.Run(kind, func(t *testing.T) {
			f := newConsoleFixture(t)
			expected := *f.d.Spec.Template.Spec.DeepCopy()
			actual := *f.p.Spec.DeepCopy()
			custom := core.Toleration{Key: "dedicated", Operator: core.TolerationOpEqual, Value: "console", Effect: core.TaintEffectNoSchedule}
			expected.Tolerations = []core.Toleration{custom}
			actual.Tolerations = append([]core.Toleration{custom}, actual.Tolerations...)
			if kind == "explicit" {
				v := int64(90)
				explicit := core.Toleration{Key: core.TaintNodeNotReady, Operator: core.TolerationOpExists, Effect: core.TaintEffectNoExecute, TolerationSeconds: &v}
				expected.Tolerations = append(expected.Tolerations, explicit)
				actual.Tolerations[1] = explicit
			}
			if kind == "wildcard" {
				wild := core.Toleration{Operator: core.TolerationOpExists}
				expected.Tolerations = append(expected.Tolerations, wild)
				actual.Tolerations = append([]core.Toleration{custom}, wild)
			}
			if !consolePodSpec(actual, expected) {
				t.Fatal("approved admission rejected")
			}
			actual.Tolerations[0].Value = "foreign"
			if consolePodSpec(actual, expected) {
				t.Fatal("approved toleration mutation accepted")
			}
		})
	}
}
func TestConsoleEndpointConvergenceRemainsBoundedAndNeverGrantsEarlyHealth(t *testing.T) {
	for _, converge := range []bool{true, false} {
		t.Run(fmt.Sprint(converge), func(t *testing.T) {
			f := newConsoleFixture(t)
			snapshot, e := f.adapter.Observe(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			reads := 0
			f.mu.Lock()
			f.tamper = func(path string, obj any) any {
				if strings.HasSuffix(path, "/endpointslices") {
					reads++
					if !converge || reads <= 2 {
						list := obj.(discovery.EndpointSliceList)
						list.Items[0] = *list.Items[0].DeepCopy()
						list.Items[0].Endpoints[0].TargetRef.UID = types.UID(consoleUID(30))
						return list
					}
				}
				return obj
			}
			f.mu.Unlock()
			if _, ready, e := f.adapter.ObserveRollout(context.Background(), snapshot); e != nil || ready {
				t.Fatal("stale route became ready or terminal", e)
			}
			limit := 4 * time.Second
			if !converge {
				limit = 80 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), limit)
			defer cancel()
			proof, e := f.adapter.WaitReady(ctx, snapshot)
			if converge {
				if e != nil || proof.PodUID != string(f.p.UID) {
					t.Fatal("did not wait for exact current Pod", e)
				}
			} else if e == nil || ctx.Err() != context.DeadlineExceeded {
				t.Fatal("persistent stale route accepted")
			}
			if f.patches != 0 {
				t.Fatal("wait changed workload")
			}
		})
	}
}

func TestConsoleRollbackRPCDoesNotOutliveOriginalClaimDeadline(t *testing.T) {
	f := newConsoleFixture(t)
	before, e := f.adapter.Observe(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	effects := &ConsoleEffects{Deployment: f.adapter, Gate: allowConsoleGate()}
	original, e := effects.Apply(context.Background(), f.plan(), before)
	if e != nil {
		t.Fatal(e)
	}
	current, e := f.adapter.Observe(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	// Simulate the last short remainder of an original claim, while the target
	// remains approved for much longer. The real TLS API delays only PATCH ACK.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	until, _ := ctx.Deadline()
	gateCalled := false
	responseDelayed := make(chan struct{})
	effects.Gate = consoleGate{func(gctx context.Context, _ ConsoleSnapshot, _ string, rollback bool) error {
		deadline, ok := gctx.Deadline()
		if !ok || deadline.After(until) || !rollback {
			return ErrTransport
		}
		gateCalled = true
		return nil
	}}
	f.tamper = func(_ string, obj any) any {
		if d, ok := obj.(apps.Deployment); ok && d.Generation == original.After.Generation+1 {
			close(responseDelayed)
			<-ctx.Done()
		}
		return obj
	}
	receipt, e := effects.Rollback(ctx, original, current)
	if !errors.Is(e, ErrConsoleUncertain) || receipt != (ConsoleReceipt{}) || !errors.Is(ctx.Err(), context.DeadlineExceeded) || !gateCalled {
		t.Fatal("RPC deadline or uncertainty was lost", e)
	}
	select {
	case <-responseDelayed:
	default:
		t.Fatal("API did not observe the conditional rollback")
	}
	if f.target.Validate(time.Now()) != nil {
		t.Fatal("fixture target expired instead of caller authority")
	}
}
