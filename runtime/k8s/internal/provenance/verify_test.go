package provenance

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/sigstore/sigstore/pkg/signature/dsse"
)

func jsonBytes(t *testing.T, v any) []byte {
	t.Helper()
	raw, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return raw
}
func uid(n int) string { return fmt.Sprintf("%08d-1111-1111-1111-111111111111", n) }
func recordFixture(t *testing.T, image string) buildrecord.Record {
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
	for id, key := range m.Policy.Keys {
		if key.Role != "build-controller" {
			inner.Keys[id] = key
		}
		switch key.Role {
		case "release-manager", "build-controller", "verifier", "deployer", "relay":
			outer.Keys[id] = key
		}
	}
	for _, g := range m.Policy.Grants {
		if authority.OuterOperation(g.Operation) {
			outer.Grants = append(outer.Grants, g)
		} else if g.Operation != "record-attempt" && g.Operation != "record-release" {
			inner.Grants = append(inner.Grants, g)
		}
	}
	s := building.Slot{ClusterUID: uid(1), NamespaceUID: uid(2), PVCUID: uid(3), PVUID: uid(4), NodeUID: uid(5), NodeBootID: uid(6), StorageIdentitySHA256: strings.Repeat("a", 64), Namespace: "ai-sdlc-builds", PVC: "build-slot-0", Node: "fixture-node", CapacityBytes: 4 << 30, InodeLimit: 32768}
	p := buildworkload.Profile{Version: "ai-sdlc.saferus-build-profile/v1", ClusterUID: s.ClusterUID, NamespaceUID: s.NamespaceUID, PublisherImage: "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("b", 64), Dependencies: map[string]buildworkload.Dependency{}, DeniedTargets: []buildworkload.DeniedTarget{{Host: "10.96.0.1", Port: 443}, {Host: "10.96.0.2", Port: 443}}}
	for i, key := range buildworkload.DependencyKeys() {
		p.Dependencies[key] = buildworkload.Dependency{UID: uid(100 + i), SpecSHA256: strings.Repeat("c", 64)}
	}
	cfg := building.Configuration{Inner: inner, Outer: outer, ProfileSHA256: p.Digest(), IntakeConfigSHA256: strings.Repeat("d", 64), IssueMaxAgeSeconds: 60, Slots: []building.Slot{s}}
	at := time.Now().Add(-time.Second)
	c := building.Command{Version: building.Version, MergeID: "merge-" + strings.Repeat("e", 64), MergeRecordSHA256: strings.Repeat("f", 64), Repository: inner.Repository, Environment: inner.Environment, IssueNumber: 4, IssueRevision: strings.Repeat("a", 64), Generation: 1, SourceSHA: strings.Repeat("b", 40), TreeSHA: strings.Repeat("c", 40), Recipe: building.Recipe, ProfileSHA256: cfg.ProfileDigest(), IntakeConfigSHA256: cfg.IntakeConfigSHA256, InnerPolicyRevision: inner.Revision(), OuterPolicyRevision: outer.Revision(), IssuedAt: at.UnixMilli(), ExpiresAt: at.Add(building.MaxAge).UnixMilli()}
	payload := jsonBytes(t, c)
	signed, e := m.Identities["release-manager"].Seal(outer, c.ID(), "build-merged", payload)
	if e != nil {
		t.Fatal(e)
	}
	compiled, e := buildworkload.Compile(cfg, p, signed, payload, s, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	observed := at.Add(100 * time.Millisecond).UnixMilli()
	term := func(phase string, n int) building.Termination {
		request := compiled.Plan.BuildRequestSHA256
		if phase == "publish" {
			request = compiled.Plan.PublishRequestSHA256
		}
		return building.Termination{Version: "ai-sdlc.build-termination/v1", Phase: phase, RequestSHA256: request, PipelineRunUID: uid(n), TaskRunUID: uid(n + 1), PodUID: uid(n + 2), NodeUID: s.NodeUID, NodeBootID: s.NodeBootID, SlotDigest: s.Digest(), PodSpecSHA256: strings.Repeat("1", 64), ObservedAt: observed, Containers: []building.ContainerExit{
			{Name: "prepare", Kind: "init", ImageDigest: "sha256:" + strings.Repeat("2", 64), ContainerID: "containerd://" + fmt.Sprintf("%064x", n), StartedAt: observed, FinishedAt: observed},
			{Name: "step", Kind: "normal", ImageDigest: "sha256:" + strings.Repeat("3", 64), ContainerID: "containerd://" + fmt.Sprintf("%064x", n+1), StartedAt: observed, FinishedAt: observed}}}
	}
	b, pub := term("build", 10), term("publish", 20)
	result := building.Result{Version: "ai-sdlc.build-result/v1", BuildID: c.ID(), PlanSHA256: compiled.Plan.Digest(), ImageURL: building.Registry, ImageDigest: image, SourceSHA: c.SourceSHA, TreeSHA: c.TreeSHA, DefinitionSHA256: compiled.Plan.DefinitionSHA256, PublisherImage: p.PublisherImage, BuildRunUID: b.PipelineRunUID, PublishRunUID: pub.PipelineRunUID, BuildObservationSHA256: b.Digest(), PublishObservationSHA256: pub.Digest()}
	r := buildrecord.Record{Context: compiled.Retained, Command: c, Plan: compiled.Plan, Build: b, Publish: pub, Result: result, RecordedAt: time.UnixMilli(observed + 1)}
	if r.Validate() != nil {
		t.Fatal("invalid synthetic historical fixture")
	}
	return r
}
func statementFixture(t *testing.T, r buildrecord.Record) map[string]any {
	t.Helper()
	obj := map[string]any{}
	if json.Unmarshal(r.Context.Publish, &obj) != nil {
		t.Fatal("compiled publisher")
	}
	requestSteps := obj["spec"].(map[string]any)["pipelineSpec"].(map[string]any)["tasks"].([]any)[0].(map[string]any)["taskSpec"].(map[string]any)["steps"].([]any)
	steps := []any{}
	for _, value := range requestSteps {
		s := value.(map[string]any)
		args := s["args"].([]any)
		for i, a := range args {
			args[i] = strings.ReplaceAll(a.(string), "$(params.run-uid)", r.Publish.PipelineRunUID)
		}
		steps = append(steps, map[string]any{"annotations": nil, "arguments": args, "entryPoint": "/component", "environment": map[string]string{"container": s["name"].(string), "image": "oci://" + s["image"].(string)}})
	}
	values := map[string]string{"IMAGE_URL": r.Result.ImageURL, "IMAGE_DIGEST": r.Result.ImageDigest, "CHAINS-GIT_URL": "https://github.com/" + r.Command.Repository, "CHAINS-GIT_COMMIT": r.Command.SourceSHA, "DEFINITION_DIGEST": "sha256:" + r.Plan.DefinitionSHA256, "RUN_UID": r.Publish.PipelineRunUID}
	results := []any{}
	for _, key := range []string{"IMAGE_URL", "IMAGE_DIGEST", "CHAINS-GIT_URL", "CHAINS-GIT_COMMIT", "DEFINITION_DIGEST", "RUN_UID"} {
		results = append(results, map[string]string{"name": key, "type": "string", "value": values[key]})
	}
	task := map[string]any{"name": "publish", "status": "Succeeded", "serviceAccountName": "publish", "results": results, "steps": steps, "invocation": map[string]any{"parameters": map[string]string{"run-uid": r.Publish.PipelineRunUID}, "environment": map[string]any{"labels": map[string]string{"tekton.dev/pipelineRunUID": r.Publish.PipelineRunUID}}}}
	publisher := strings.Split(r.Result.PublisherImage, "@sha256:")
	return map[string]any{"_type": "https://in-toto.io/Statement/v0.1", "predicateType": PredicateType, "subject": []any{map[string]any{"name": r.Result.ImageURL, "digest": map[string]string{"sha256": strings.TrimPrefix(r.Result.ImageDigest, "sha256:")}}}, "predicate": map[string]any{"buildType": "tekton.dev/v1/PipelineRun", "builder": map[string]string{"id": "https://tekton.dev/chains/v2"}, "buildConfig": map[string]any{"tasks": []any{task}}, "materials": []material{{"oci://" + publisher[0], map[string]string{"sha256": publisher[1]}}, {"git+https://github.com/" + r.Command.Repository + ".git", map[string]string{"sha1": r.Command.SourceSHA}}}}}
}

type signatureFixture struct {
	record                   buildrecord.Record
	trust                    Trust
	credential               Credential
	signer                   signature.SignerVerifier
	routes                   map[string][]byte
	imagePayload             []byte
	statement                map[string]any
	sigManifest, attManifest string
}

func newFixture(t *testing.T) *signatureFixture {
	t.Helper()
	image := jsonBytes(t, manifest{SchemaVersion: 2, MediaType: ociManifest, Config: descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: "sha256:" + strings.Repeat("1", 64), Size: 2}, Layers: []descriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: "sha256:" + strings.Repeat("2", 64), Size: 100}}})
	r := recordFixture(t, digest(image))
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	signer, e := signature.LoadECDSASignerVerifier(key, crypto.SHA256)
	if e != nil {
		t.Fatal(e)
	}
	spki, e := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if e != nil {
		t.Fatal(e)
	}
	trust := Trust{Version: PolicyVersion, Scope: LocalScope, Repository: r.Command.Repository, Environment: r.Command.Environment, ClusterUID: r.Context.Profile.ClusterUID, BuildNamespaceUID: r.Context.Profile.NamespaceUID, Registry: building.Registry, SignerPublicKey: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki})), SignerSHA256: authority.Digest(spki), PublisherImage: r.Result.PublisherImage, VerifierImage: "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("e", 64), VerifierVersion: VerifierVersion, CompilerVersion: buildworkload.CompilerVersion, PredicateType: PredicateType, Transparency: LocalTransparency, ApprovedAt: time.Now().Add(-time.Minute).UnixMilli(), ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	payload := jsonBytes(t, map[string]any{"critical": map[string]any{"type": "cosign container image signature", "identity": map[string]string{"docker-reference": r.Result.ImageURL}, "image": map[string]string{"docker-manifest-digest": r.Result.ImageDigest}}, "optional": nil})
	f := &signatureFixture{record: r, trust: trust, credential: Credential{"ai-sdlc-verifier", strings.Repeat("x", 40)}, signer: signer, routes: map[string][]byte{registryPath + "manifests/" + r.Result.ImageDigest: image}, imagePayload: payload, statement: statementFixture(t, r)}
	f.resign(t)
	return f
}
func (f *signatureFixture) resign(t *testing.T) {
	t.Helper()
	sig, e := f.signer.SignMessage(bytes.NewReader(f.imagePayload))
	if e != nil {
		t.Fatal(e)
	}
	envelope, e := dsse.WrapSigner(f.signer, "application/vnd.in-toto+json").SignMessage(bytes.NewReader(jsonBytes(t, f.statement)))
	if e != nil {
		t.Fatal(e)
	}
	f.sigManifest = f.layer(t, "sig", signatureMedia, f.imagePayload, map[string]string{"dev.cosignproject.cosign/signature": base64.StdEncoding.EncodeToString(sig)})
	f.attManifest = f.layer(t, "att", attestationMedia, envelope, nil)
}
func (f *signatureFixture) layer(t *testing.T, suffix, media string, data []byte, annotations map[string]string) string {
	t.Helper()
	blob := digest(data)
	f.routes[registryPath+"blobs/"+blob] = data
	path := registryPath + "manifests/sha256-" + strings.TrimPrefix(f.record.Result.ImageDigest, "sha256:") + "." + suffix
	f.routes[path] = jsonBytes(t, manifest{SchemaVersion: 2, MediaType: ociManifest, Config: descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: "sha256:" + strings.Repeat("0", 64), Size: 2}, Layers: []descriptor{{MediaType: media, Digest: blob, Size: int64(len(data)), Annotations: annotations}}})
	return path
}
func tlsFixture(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated provenance test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caRaw, e := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{registryHost}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	cert, e := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	return tls.Certificate{Certificate: [][]byte{cert, caRaw}, PrivateKey: key}, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caRaw}))
}
func (f *signatureFixture) serve(t *testing.T, hook func(http.ResponseWriter, *http.Request) bool) (*Verifier, *atomic.Int32) {
	t.Helper()
	cert, ca := tlsFixture(t)
	f.trust.RegistryCA = ca
	calls := &atomic.Int32{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		user, password, ok := r.BasicAuth()
		if !ok || user != f.credential.Username || password != f.credential.Password || r.Method != "GET" || r.Host != registryHost {
			t.Error("unexpected registry identity or method")
			w.WriteHeader(403)
			return
		}
		if hook != nil && hook(w, r) {
			return
		}
		raw, ok := f.routes[r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Write(raw)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(server.Close)
	v, e := NewVerifier(f.trust, f.credential)
	if e != nil {
		t.Fatal(e)
	}
	transport := v.registry.client.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != registryHost+":443" {
			return nil, ErrProvenance
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	return v, calls
}

func TestRetainedProvenanceTLSAndCryptographicBinding(t *testing.T) {
	f := newFixture(t)
	v, calls := f.serve(t, nil)
	proof, e := v.Verify(context.Background(), f.record)
	if e != nil || proof.ReleaseAuthorized || proof.BuildRecordSHA256 != f.record.Digest() || proof.TrustSHA256 != f.trust.Digest() || proof.ImageDigest != f.record.Result.ImageDigest || calls.Load() != 5 {
		t.Fatalf("closed proof: %v (requests=%d imageBinding=%v statementBinding=%v)", e, calls.Load(), bindImage(f.imagePayload, f.record), bindStatement(jsonBytes(t, f.statement), f.record))
	}
	if proof.SignatureManifestDigest != digest(f.routes[f.sigManifest]) || proof.AttestationManifestDigest != digest(f.routes[f.attManifest]) {
		t.Fatal("mutable reference was not pinned to exact evidence")
	}
	if _, e = v.Verify(context.Background(), f.record); e == nil || calls.Load() != 5 {
		t.Fatal("verifier replay")
	}
}
func TestRetainedProvenanceRejectsSignedSubstitutions(t *testing.T) {
	for _, name := range []string{"image-subject", "image-alias", "attestation-subject", "source", "tree", "definition", "run", "publisher", "builder", "extra-task", "extra-step", "material", "duplicate-result", "wrong-key", "unknown-version", "production-scope", "expired-trust", "wrong-cluster"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			p := f.statement["predicate"].(map[string]any)
			task := p["buildConfig"].(map[string]any)["tasks"].([]any)[0].(map[string]any)
			switch name {
			case "image-subject":
				f.imagePayload = bytes.ReplaceAll(f.imagePayload, []byte(f.record.Result.ImageDigest), []byte("sha256:"+strings.Repeat("f", 64)))
			case "image-alias":
				f.imagePayload = bytes.Replace(f.imagePayload, []byte(`"critical"`), []byte(`"Critical"`), 1)
			case "attestation-subject":
				f.statement["subject"].([]any)[0].(map[string]any)["name"] = "attacker.invalid/other"
			case "source", "definition", "run":
				wanted := map[string]string{"source": "CHAINS-GIT_COMMIT", "definition": "DEFINITION_DIGEST", "run": "RUN_UID"}[name]
				for _, r := range task["results"].([]any) {
					row := r.(map[string]string)
					if row["name"] == wanted {
						row["value"] = "changed"
					}
				}
			case "tree":
				f.record.Command.TreeSHA = strings.Repeat("f", 40)
			case "publisher":
				task["steps"].([]any)[0].(map[string]any)["environment"].(map[string]string)["image"] = "oci://attacker.invalid/tool"
			case "builder":
				p["builder"] = map[string]string{"id": "attacker.invalid"}
			case "extra-task":
				p["buildConfig"].(map[string]any)["tasks"] = []any{task, task}
			case "extra-step":
				task["steps"] = append(task["steps"].([]any), task["steps"].([]any)[0])
			case "material":
				p["materials"].([]material)[0].URI = "oci://other"
			case "duplicate-result":
				task["results"] = append(task["results"].([]any), task["results"].([]any)[0])
			case "wrong-key":
				other := newFixture(t)
				f.trust.SignerPublicKey = other.trust.SignerPublicKey
				f.trust.SignerSHA256 = other.trust.SignerSHA256
			case "unknown-version":
				f.trust.VerifierVersion = "unknown"
			case "production-scope":
				f.trust.Scope = "production"
			case "expired-trust":
				f.trust.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
			case "wrong-cluster":
				f.trust.ClusterUID = uid(999)
			}
			f.resign(t)
			if name == "unknown-version" || name == "production-scope" || name == "expired-trust" {
				_, ca := tlsFixture(t)
				f.trust.RegistryCA = ca
				if _, e := NewVerifier(f.trust, f.credential); e == nil {
					t.Fatal("unsupported trust accepted")
				}
				return
			}
			v, _ := f.serve(t, nil)
			if _, e := v.Verify(context.Background(), f.record); e == nil {
				t.Fatal("signed substitution accepted")
			}
		})
	}
}
func TestRetainedProvenanceTransportDenials(t *testing.T) {
	for _, name := range []string{"redirect", "compressed", "oversized", "truncated", "unauthorized", "missing", "bad-blob", "ambiguous", "wrong-size", "bad-signature", "duplicate-json"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if name == "ambiguous" || name == "wrong-size" || name == "bad-signature" {
				var m manifest
				if json.Unmarshal(f.routes[f.sigManifest], &m) != nil {
					t.Fatal("manifest")
				}
				if name == "ambiguous" {
					m.Layers = append(m.Layers, m.Layers[0])
				}
				if name == "wrong-size" {
					m.Layers[0].Size++
				}
				if name == "bad-signature" {
					m.Layers[0].Annotations["dev.cosignproject.cosign/signature"] = base64.StdEncoding.EncodeToString([]byte("invalid"))
				}
				f.routes[f.sigManifest] = jsonBytes(t, m)
			}
			if name == "duplicate-json" {
				f.routes[f.sigManifest] = bytes.Replace(f.routes[f.sigManifest], []byte(`"schemaVersion":2`), []byte(`"schemaVersion":2,"schemaVersion":2`), 1)
			}
			v, calls := f.serve(t, func(w http.ResponseWriter, r *http.Request) bool {
				switch name {
				case "redirect":
					w.Header().Set("Location", "https://"+registryHost+"/unexpected")
					w.WriteHeader(302)
					return true
				case "compressed":
					w.Header().Set("Content-Encoding", "gzip")
					w.Write(f.routes[r.URL.Path])
					return true
				case "oversized":
					w.Header().Set("Content-Length", fmt.Sprint(manifestLimit+1))
					w.Write(bytes.Repeat([]byte{'x'}, manifestLimit+1))
					return true
				case "truncated":
					w.Header().Set("Content-Length", "1000")
					w.Write([]byte("short"))
					return true
				case "unauthorized":
					w.WriteHeader(401)
					return true
				case "missing":
					w.WriteHeader(404)
					return true
				case "bad-blob":
					if strings.Contains(r.URL.Path, "/blobs/") {
						w.Write([]byte("changed"))
						return true
					}
				}
				return false
			})
			if _, e := v.Verify(context.Background(), f.record); e == nil {
				t.Fatal("bad evidence accepted")
			}
			if calls.Load() > 5 {
				t.Fatal("unbounded requests")
			}
			if name == "redirect" || name == "unauthorized" {
				if calls.Load() != 1 {
					t.Fatal("implicit redirect or retry")
				}
			}
		})
	}
}
func TestRetainedProvenanceSingleUseConcurrentAndCancelled(t *testing.T) {
	f := newFixture(t)
	v, calls := f.serve(t, nil)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := v.Verify(context.Background(), f.record); e == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 || calls.Load() != 5 {
		t.Fatal("concurrent reuse")
	}
	f = newFixture(t)
	v, calls = f.serve(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := v.Verify(ctx, f.record); e == nil || calls.Load() != 0 {
		t.Fatal("cancelled verification used network")
	}
}

func TestRetainedProvenanceRejectsInvalidDSSE(t *testing.T) {
	for _, name := range []string{"signature", "payload", "type", "multiple", "alias", "unknown", "media", "foreign-subject"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			var m manifest
			if json.Unmarshal(f.routes[f.attManifest], &m) != nil {
				t.Fatal("fixture manifest")
			}
			var env map[string]any
			if json.Unmarshal(f.routes[registryPath+"blobs/"+m.Layers[0].Digest], &env) != nil {
				t.Fatal("fixture envelope")
			}
			switch name {
			case "signature":
				env["signatures"].([]any)[0].(map[string]any)["sig"] = base64.StdEncoding.EncodeToString([]byte("invalid"))
			case "payload":
				env["payload"] = base64.StdEncoding.EncodeToString([]byte(`{}`))
			case "type":
				env["payloadType"] = "other"
			case "multiple":
				env["signatures"] = append(env["signatures"].([]any), env["signatures"].([]any)[0])
			case "alias":
				env["PayloadType"] = env["payloadType"]
				delete(env, "payloadType")
			case "unknown":
				env["authority"] = "approved"
			}
			f.layer(t, "att", attestationMedia, jsonBytes(t, env), nil)
			if name == "media" || name == "foreign-subject" {
				if json.Unmarshal(f.routes[f.attManifest], &m) != nil {
					t.Fatal("manifest")
				}
				if name == "media" {
					m.Layers[0].MediaType = "application/json"
				} else {
					m.Subject = &m.Config
				}
				f.routes[f.attManifest] = jsonBytes(t, m)
			}
			v, calls := f.serve(t, nil)
			if _, err := v.Verify(context.Background(), f.record); err == nil {
				t.Fatal("invalid DSSE accepted")
			}
			if calls.Load() < 4 || calls.Load() > 5 {
				t.Fatal("test did not reach DSSE boundary")
			}
		})
	}
}

func TestRetainedProvenanceIndependentTrustAndCancellation(t *testing.T) {
	for _, name := range []string{"bad-ca", "ca-prefix", "key-prefix", "key-hash", "credential", "inflight-cancel", "expires-during-read"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			expires := time.Now().Add(time.Second)
			var observed atomic.Int32
			v, calls := f.serve(t, func(w http.ResponseWriter, r *http.Request) bool {
				if name == "inflight-cancel" {
					cancel()
				}
				if name == "expires-during-read" && observed.Add(1) == 5 {
					time.Sleep(time.Until(expires.Add(time.Millisecond)))
				}
				return false
			})
			switch name {
			case "bad-ca":
				v.registry.client.Transport.(*http.Transport).TLSClientConfig.RootCAs = x509.NewCertPool()
			case "ca-prefix":
				f.trust.RegistryCA = "ignored prefix\n" + f.trust.RegistryCA
			case "key-prefix":
				f.trust.SignerPublicKey = "ignored prefix\n" + f.trust.SignerPublicKey
			case "key-hash":
				f.trust.SignerSHA256 = strings.Repeat("0", 64)
			case "credential":
				f.credential.Username = "publisher"
			case "expires-during-read":
				v.trust.ExpiresAt = expires.UnixMilli()
			}
			if name == "ca-prefix" || name == "key-prefix" || name == "key-hash" || name == "credential" {
				if _, err := NewVerifier(f.trust, f.credential); err == nil {
					t.Fatal("invalid trust accepted")
				}
				if calls.Load() != 0 {
					t.Fatal("invalid trust reached registry")
				}
				return
			}
			if _, err := v.Verify(ctx, f.record); err == nil {
				t.Fatal("trust or cancellation bypass")
			}
			if name == "expires-during-read" && calls.Load() != 5 {
				t.Fatal("expiry test did not reach final trust check")
			}
			if name == "bad-ca" && calls.Load() != 0 {
				t.Fatal("credential crossed untrusted TLS")
			}
			if name == "inflight-cancel" && calls.Load() != 1 {
				t.Fatal("continued after cancellation")
			}
		})
	}
}
