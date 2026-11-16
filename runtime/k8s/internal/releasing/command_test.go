package releasing

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenancerecord"
	"math/big"
	"strings"
	"testing"
	"time"
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
	r, _, _ := recordAndAuthorityFixture(t, image)
	return r
}
func recordAndAuthorityFixture(t *testing.T, image string) (buildrecord.Record, building.Configuration, authority.BootstrapMaterial) {
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
	return r, cfg, m
}

// Synthetic journal-contract fixture, never cryptographic or deployment proof.
// Actual Sigstore/DSSE verification is exercised in package provenance.
func releaseFixture(t *testing.T) (Configuration, provenancerecord.Record, buildrecord.Record, authority.BootstrapMaterial, time.Time) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	b, cfg, m := recordAndAuthorityFixture(t, "sha256:"+strings.Repeat("a", 64))
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	spki, e := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if e != nil {
		t.Fatal(e)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "disposable-release-fixture"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, e := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	trust := provenance.Trust{Version: provenance.PolicyVersion, Scope: provenance.LocalScope, Repository: b.Command.Repository, Environment: b.Command.Environment, ClusterUID: b.Context.Profile.ClusterUID, BuildNamespaceUID: b.Context.Profile.NamespaceUID, Registry: building.Registry, RegistryCA: ca, SignerPublicKey: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki})), SignerSHA256: authority.Digest(spki), PublisherImage: b.Result.PublisherImage, VerifierImage: "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("e", 64), VerifierVersion: provenance.VerifierVersion, CompilerVersion: buildworkload.CompilerVersion, PredicateType: provenance.PredicateType, Transparency: provenance.LocalTransparency, ApprovedAt: now.Add(-time.Hour).UnixMilli(), ExpiresAt: now.Add(2 * time.Hour).UnixMilli()}
	pc := provenance.Configuration{Build: cfg, Trust: trust}
	verify, e := provenance.NewCommand(pc, b, now)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := b.Context.Encode()
	proof := provenance.Proof{Version: provenance.PolicyVersion, Scope: provenance.LocalScope, BuildRecordSHA256: b.Digest(), RetainedContextSHA256: authority.Digest(raw), TrustSHA256: trust.Digest(), VerifierImage: trust.VerifierImage, VerifierVersion: trust.VerifierVersion, ImageDigest: b.Result.ImageDigest, SignatureManifestDigest: "sha256:" + strings.Repeat("1", 64), SignatureBlobDigest: "sha256:" + strings.Repeat("2", 64), AttestationManifestDigest: "sha256:" + strings.Repeat("3", 64), AttestationBlobDigest: "sha256:" + strings.Repeat("4", 64), BuildObservationSHA256: b.Build.Digest(), PublishObservationSHA256: b.Publish.Digest(), Transparency: provenance.LocalTransparency}
	r := provenancerecord.Record{Command: verify, Configuration: pc, Proof: proof, RecordedAt: now}
	target := Target{Version: TargetVersion, Scope: provenance.LocalScope, Repository: trust.Repository, Environment: trust.Environment, ClusterUID: trust.ClusterUID, NamespaceUID: uid(200), Namespace: trust.Environment, Deployment: "saferus-console", DeploymentUID: uid(201), Container: "console", DeploymentSpecSHA256: strings.Repeat("5", 64), Service: "saferus-console", ServiceUID: uid(202), ServiceSpecSHA256: strings.Repeat("6", 64), PullSecret: "ai-sdlc-release-reader", PullSecretUID: uid(203), TLSSecretUID: uid(204), TLSCA: ca, TLSHostname: "saferus-console.ai-sdlc-production.svc", BaselineImage: "localhost/saferus-console@sha256:" + strings.Repeat("7", 64), RolloutSeconds: 300, SmokeSeconds: 30, ApprovedAt: now.Add(-time.Minute).UnixMilli(), ExpiresAt: now.Add(time.Hour).UnixMilli()}
	return Configuration{Provenance: pc, Target: target}, r, b, m, now
}
func TestPromotionHasFreshFiniteAuthorityForExpiredHistory(t *testing.T) {
	cfg, r, b, m, at := releaseFixture(t)
	now := at.Add(21 * time.Minute)
	c, e := NewCommand(cfg, r, b, now)
	if e != nil {
		t.Fatal(e)
	}
	raw := jsonBytes(t, c)
	receipt, e := m.Identities["verifier"].Seal(cfg.Provenance.Build.Outer, c.ID(), "promote-artifact", raw)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = VerifyCommand(cfg, receipt, raw, now); e != nil {
		t.Fatal(e)
	}
	if c.BindRecords(cfg, r, b, now) != nil {
		t.Fatal("valid history")
	}
	if _, e = VerifyCommand(cfg, receipt, raw, time.UnixMilli(c.ExpiresAt)); e == nil {
		t.Fatal("expiry accepted")
	}
	if c.BindRecords(cfg, r, b, time.UnixMilli(c.ExpiresAt)) == nil {
		t.Fatal("history renewed authority")
	}
}
func TestPromotionRejectsSubstitutionRevocationAndAmbiguousPayload(t *testing.T) {
	for _, name := range []string{"freeze-inner", "freeze-outer", "target", "trust", "profile", "intake", "key-revoked", "record", "build", "image", "source", "issue", "generation", "future", "budget", "expired-target", "expiry-beyond-trust", "alias", "unknown", "missing", "duplicate", "wrong-role", "historical-release", "future-record"} {
		t.Run(name, func(t *testing.T) {
			cfg, r, b, m, now := releaseFixture(t)
			c, e := NewCommand(cfg, r, b, now)
			if e != nil {
				t.Fatal(e)
			}
			raw := jsonBytes(t, c)
			switch name {
			case "image":
				c.ImageDigest = "sha256:" + strings.Repeat("f", 64)
			case "source":
				c.SourceSHA = strings.Repeat("f", 40)
			case "issue":
				c.IssueRevision = strings.Repeat("f", 64)
			case "generation":
				c.Generation++
			case "future":
				c.IssuedAt = now.Add(10 * time.Second).UnixMilli()
			case "budget":
				c.ExpiresAt = now.Add(3 * time.Minute).UnixMilli()
			case "expired-target":
				cfg.Target.ExpiresAt = now.UnixMilli()
			case "expiry-beyond-trust":
				cfg.Provenance.Trust.ExpiresAt = now.Add(time.Second).UnixMilli()
			}
			raw = jsonBytes(t, c)
			switch name {
			case "alias":
				raw = bytes.Replace(raw, []byte(`"targetSHA256"`), []byte(`"TargetSHA256"`), 1)
			case "unknown":
				raw = append(raw[:len(raw)-1], []byte(`,"url":"https://untrusted.invalid"}`)...)
			case "duplicate":
				raw = append(raw[:len(raw)-1], []byte(`,"version":"duplicate"}`)...)
			case "missing":
				var obj map[string]any
				_ = json.Unmarshal(raw, &obj)
				delete(obj, "generation")
				raw = jsonBytes(t, obj)
			}
			key := m.Identities["verifier"]
			op := "promote-artifact"
			if name == "wrong-role" {
				key = m.Identities["build-controller"]
				op = "verify-built"
			}
			signed, e := key.Seal(cfg.Provenance.Build.Outer, c.ID(), op, raw)
			if e != nil {
				t.Fatal(e)
			}
			switch name {
			case "freeze-inner":
				cfg.Provenance.Build.Inner.Freeze = true
			case "freeze-outer":
				cfg.Provenance.Build.Outer.Freeze = true
			case "target":
				cfg.Target.DeploymentUID = uid(300)
			case "trust":
				cfg.Provenance.Trust.ExpiresAt--
			case "profile":
				cfg.Provenance.Build.IssueMaxAgeSeconds++
			case "intake":
				cfg.Provenance.Build.IntakeConfigSHA256 = strings.Repeat("f", 64)
			case "key-revoked":
				delete(cfg.Provenance.Build.Outer.Keys, key.KeyID)
			case "record":
				r.RecordedAt = r.RecordedAt.Add(-time.Millisecond)
			case "build":
				b.RecordedAt = b.RecordedAt.Add(-time.Millisecond)
			case "historical-release":
				r.Proof.ReleaseAuthorized = true
			case "future-record":
				r.RecordedAt = now.Add(time.Millisecond)
			}
			switch name {
			case "record", "build", "image", "source", "issue", "generation", "historical-release", "future-record":
				if c.BindRecords(cfg, r, b, now) == nil {
					t.Fatal("substituted evidence accepted")
				}
				return
			}
			if _, e = VerifyCommand(cfg, signed, raw, now); e == nil {
				t.Fatal("invalid authority accepted")
			}
		})
	}
}
func TestInstalledTargetRejectsEffectAndTrustExpansion(t *testing.T) {
	for _, name := range []string{"cluster", "duplicate-uid", "namespace", "workload", "container", "service", "secret", "hostname", "registry", "tag", "spec", "ca", "nonca", "budget", "scope", "expiry", "expired"} {
		t.Run(name, func(t *testing.T) {
			cfg, _, _, _, now := releaseFixture(t)
			x := cfg.Target
			switch name {
			case "cluster":
				x.ClusterUID = "missing"
			case "duplicate-uid":
				x.PullSecretUID = x.ServiceUID
			case "namespace":
				x.Namespace = "production"
			case "workload":
				x.Deployment = "vault"
			case "container":
				x.Container = "vault"
			case "service":
				x.Service = "vault"
			case "secret":
				x.PullSecret = "publisher"
			case "hostname":
				x.TLSHostname = "attacker.invalid"
			case "registry":
				x.BaselineImage = "attacker.invalid/x@sha256:" + strings.Repeat("7", 64)
			case "tag":
				x.BaselineImage = "localhost/saferus-console:latest"
			case "spec":
				x.DeploymentSpecSHA256 = ""
			case "ca":
				x.TLSCA = "prefix" + x.TLSCA
			case "nonca":
				x.TLSCA = cfg.Provenance.Trust.SignerPublicKey
			case "budget":
				x.RolloutSeconds++
			case "scope":
				x.Scope = "enterprise-production"
			case "expiry":
				x.ExpiresAt = now.Add(25 * time.Hour).UnixMilli()
			case "expired":
				x.ExpiresAt = now.UnixMilli()
			}
			if x.Validate(now) == nil {
				t.Fatal("expanded target accepted")
			}
		})
	}
}

func TestProvenanceRecordAliasPreservesHistoricalDigest(t *testing.T) {
	_, r, _, _, _ := releaseFixture(t)
	legacy := struct {
		Command       provenance.Command
		Configuration provenance.Configuration
		Proof         provenance.Proof
		RecordedAt    time.Time
	}{r.Command, r.Configuration, r.Proof, r.RecordedAt}
	if !bytes.Equal(jsonBytes(t, legacy), jsonBytes(t, r)) || authority.Digest(jsonBytes(t, legacy)) != r.Digest() {
		t.Fatal("historical record encoding changed")
	}
}
func TestPromotionCannotOutliveInstalledTrustAndTarget(t *testing.T) {
	for _, name := range []string{"target", "trust"} {
		t.Run(name, func(t *testing.T) {
			cfg, r, b, _, now := releaseFixture(t)
			// Create history under this exact short-lived installed trust. A different
			// trust document cannot silently authorize an old provenance record.
			limit := now.Add(100 * time.Second).UnixMilli()
			if name == "target" {
				cfg.Target.ExpiresAt = limit
			} else {
				cfg.Provenance.Trust.ExpiresAt = limit
				r.Configuration = cfg.Provenance
				r.Command.TrustSHA256 = cfg.Provenance.Trust.Digest()
				r.Proof.TrustSHA256 = r.Command.TrustSHA256
				r.Command.ExpiresAt = limit
			}
			c, e := NewCommand(cfg, r, b, now)
			if e != nil || c.ExpiresAt != limit {
				t.Fatal("finite installed trust ignored", e)
			}
			if _, e = NewCommand(cfg, r, b, now.Add(11*time.Second)); e == nil {
				t.Fatal("insufficient decision budget accepted")
			}
		})
	}
}
