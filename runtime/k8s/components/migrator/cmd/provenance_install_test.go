package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
	"golang.org/x/crypto/bcrypt"
)

func provenanceInstallationFixture(t *testing.T) (provenance.Trust, buildworkload.Profile) {
	t.Helper()
	profile := installedFixture(t).Profile
	profile.PublisherImage = "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("a", 64)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	trust := provenance.Trust{Version: provenance.PolicyVersion, Scope: provenance.LocalScope, Repository: "os-santiago/saferus",
		Environment: "ai-sdlc-production", ClusterUID: profile.ClusterUID, BuildNamespaceUID: profile.NamespaceUID,
		Registry: "registry-service.kind-registry/ai-sdlc-fixtures/saferus", RegistryCA: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		SignerPublicKey: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public})), SignerSHA256: authority.Digest(public),
		PublisherImage: profile.PublisherImage, VerifierImage: "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("b", 64),
		VerifierVersion: provenance.VerifierVersion, CompilerVersion: "ai-sdlc.saferus-build-compiler/v1", PredicateType: provenance.PredicateType,
		Transparency: provenance.LocalTransparency, ApprovedAt: now.Add(-time.Second).UnixMilli(), ExpiresAt: now.Add(time.Hour).UnixMilli()}
	return trust, profile
}

func TestProvenanceInstallationCompilerBindsIndependentTrustAndPins(t *testing.T) {
	trust, profile := provenanceInstallationFixture(t)
	input := map[string]any{"trust": trust, "profile": profile, "verifierImage": trust.VerifierImage}
	raw, _ := json.Marshal(input)
	var output bytes.Buffer
	if err := compileProvenanceInstallation(bytes.NewReader(raw), &output); err != nil {
		t.Fatal(err)
	}
	var actual struct {
		Trust         provenance.Trust `json:"trust"`
		TrustSHA256   string           `json:"trustSHA256"`
		ProfileSHA256 string           `json:"profileSHA256"`
	}
	if json.Unmarshal(output.Bytes(), &actual) != nil || actual.Trust != trust || actual.TrustSHA256 != trust.Digest() || actual.ProfileSHA256 != profile.Digest() {
		t.Fatal("compiler changed independently supplied trust or profile")
	}
	for _, name := range []string{"unknown", "image", "cluster", "namespace", "publisher", "key", "ca", "expired", "profile", "trust-alias", "field-alias", "profile-alias", "dependency-alias", "target-alias"} {
		t.Run(name, func(t *testing.T) {
			var changed map[string]any
			_ = json.Unmarshal(raw, &changed)
			c := changed["trust"].(map[string]any)
			switch name {
			case "unknown":
				changed["approveObserved"] = true
			case "image":
				changed["verifierImage"] = profile.PublisherImage
			case "cluster":
				c["clusterUID"] = "22222222-2222-2222-2222-222222222222"
			case "namespace":
				c["buildNamespaceUID"] = "22222222-2222-2222-2222-222222222222"
			case "publisher":
				c["publisherImage"] = trust.VerifierImage
			case "key":
				c["signerSHA256"] = strings.Repeat("c", 64)
			case "ca":
				c["registryCA"] = trust.SignerPublicKey
			case "expired":
				c["expiresAt"] = time.Now().Add(-time.Second).UnixMilli()
			case "profile":
				changed["profile"].(map[string]any)["dependencies"] = map[string]any{}
			case "trust-alias":
				c["Registry"] = c["registry"]
				delete(c, "registry")
			case "field-alias":
				changed["Trust"] = changed["trust"]
				delete(changed, "trust")
			case "profile-alias":
				p := changed["profile"].(map[string]any)
				p["ClusterUID"] = p["clusterUID"]
				delete(p, "clusterUID")
			case "dependency-alias":
				p := changed["profile"].(map[string]any)["dependencies"].(map[string]any)
				for _, value := range p {
					d := value.(map[string]any)
					d["UID"] = d["uid"]
					delete(d, "uid")
					break
				}
			case "target-alias":
				d := changed["profile"].(map[string]any)["deniedTargets"].([]any)[0].(map[string]any)
				d["Host"] = d["host"]
				delete(d, "host")
			}
			bad, _ := json.Marshal(changed)
			output.Reset()
			if compileProvenanceInstallation(bytes.NewReader(bad), &output) == nil || output.Len() != 0 {
				t.Fatal("invalid input produced approval-shaped output")
			}
		})
	}
}

func TestProvenanceCustodyPreservesPublisherAndRecoversWithoutRotation(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("synthetic-existing-publisher-only"), 10)
	if err != nil {
		t.Fatal(err)
	}
	publisher := "konflux:" + string(hash) + "\n"
	input := map[string]any{"version": custodyVersion, "htpasswd": publisher}
	raw, _ := json.Marshal(input)
	var output bytes.Buffer
	if prepareProvenanceRegistryCustody(bytes.NewReader(raw), &output) != nil {
		t.Fatal("fixture credential preparation failed")
	}
	var custody registryCustody
	if json.Unmarshal(output.Bytes(), &custody) != nil || custody.Version != custodyVersion || !strings.HasPrefix(custody.HTPasswd, publisher) ||
		custody.Credential.Username != "ai-sdlc-verifier" || len(custody.Credential.Password) != 64 ||
		bcrypt.CompareHashAndPassword([]byte(custody.ReaderHash), []byte(custody.Credential.Password)) != nil {
		t.Fatal("invalid generated custody")
	}
	expected := append([]byte(nil), output.Bytes()...)
	for _, current := range []string{publisher, custody.HTPasswd} {
		input = map[string]any{"version": custodyVersion, "htpasswd": current, "credential": custody.Credential, "readerHash": custody.ReaderHash}
		raw, _ = json.Marshal(input)
		output.Reset()
		if prepareProvenanceRegistryCustody(bytes.NewReader(raw), &output) != nil || !bytes.Equal(output.Bytes(), expected) {
			t.Fatal("retry rotated recorded custody")
		}
	}
	for _, name := range []string{"unknown", "new-user", "reordered", "duplicate", "unrecorded-reader", "foreign-reader", "changed-password", "changed-user", "weak-reader", "weak-publisher", "unterminated", "oversize", "duplicate-field", "field-alias", "credential-alias", "null-custody", "orphan-hash"} {
		t.Run(name, func(t *testing.T) {
			input := map[string]any{"version": custodyVersion, "htpasswd": custody.HTPasswd, "credential": custody.Credential, "readerHash": custody.ReaderHash}
			switch name {
			case "unknown":
				input["run"] = true
			case "new-user":
				input["htpasswd"] = custody.HTPasswd + "admin:" + string(hash) + "\n"
			case "reordered":
				input["htpasswd"] = "ai-sdlc-verifier:" + custody.ReaderHash + "\n" + publisher
			case "duplicate":
				input["htpasswd"] = publisher + publisher
			case "unrecorded-reader":
				delete(input, "credential")
				delete(input, "readerHash")
			case "foreign-reader":
				input["htpasswd"] = publisher + "ai-sdlc-verifier:" + string(hash) + "\n"
			case "changed-password":
				input["credential"] = registryCredential{"ai-sdlc-verifier", strings.Repeat("0", 64)}
			case "changed-user":
				input["credential"] = registryCredential{"konflux", custody.Credential.Password}
			case "weak-reader":
				input["readerHash"] = string(hash)
			case "weak-publisher":
				input["htpasswd"] = strings.Replace(custody.HTPasswd, "$10$", "$04$", 1)
			case "unterminated":
				input["htpasswd"] = strings.TrimSuffix(custody.HTPasswd, "\n")
			case "oversize":
				input["htpasswd"] = strings.Repeat("x", 4097)
			case "field-alias":
				input["Version"] = input["version"]
				delete(input, "version")
			case "credential-alias":
				input["credential"] = map[string]string{"Username": "ai-sdlc-verifier", "password": custody.Credential.Password}
			case "null-custody":
				input["htpasswd"] = publisher
				input["credential"] = nil
				input["readerHash"] = nil
			case "orphan-hash":
				input["htpasswd"] = publisher
				delete(input, "credential")
				input["readerHash"] = ""
			}
			raw, _ := json.Marshal(input)
			if name == "duplicate-field" {
				raw = append([]byte(`{"version":"ignored",`), raw[1:]...)
			}
			output.Reset()
			if prepareProvenanceRegistryCustody(bytes.NewReader(raw), &output) == nil || output.Len() != 0 {
				t.Fatal("invalid custody accepted")
			}
		})
	}
}
