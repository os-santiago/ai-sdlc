// Package provenance verifies retained signatures for the closed local Saferus
// recipe. Its proofs are inputs to a separately authorized journal, never
// promotion or deployment permissions.
package provenance

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildrecord"
	"github.com/sigstore/sigstore/pkg/signature"
)

const PolicyVersion = "ai-sdlc.saferus-provenance/v1"
const VerifierVersion = "sigstore/v1.11.3"
const PredicateType = "https://slsa.dev/provenance/v0.2"
const LocalScope = "kind-test-production"
const LocalTransparency = "local-key-without-transparency-log/v1"
const MaxEvidenceBytes = 2 << 20

var ErrProvenance = errors.New("retained artifact provenance denied")
var sha256RE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var uuidRE = regexp.MustCompile(`^[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}$`)

// Trust is public, independently reviewed installation input. Registry evidence
// and issue/model content cannot supply or approve this configuration.
type Trust struct {
	Version           string `json:"version"`
	Scope             string `json:"scope"`
	Repository        string `json:"repository"`
	Environment       string `json:"environment"`
	ClusterUID        string `json:"clusterUID"`
	BuildNamespaceUID string `json:"buildNamespaceUID"`
	Registry          string `json:"registry"`
	RegistryCA        string `json:"registryCA"`
	SignerPublicKey   string `json:"signerPublicKey"`
	SignerSHA256      string `json:"signerSHA256"`
	PublisherImage    string `json:"publisherImage"`
	VerifierImage     string `json:"verifierImage"`
	VerifierVersion   string `json:"verifierVersion"`
	CompilerVersion   string `json:"compilerVersion"`
	PredicateType     string `json:"predicateType"`
	Transparency      string `json:"transparency"`
	ApprovedAt        int64  `json:"approvedAt"`
	ExpiresAt         int64  `json:"expiresAt"`
}

func (t Trust) Digest() string { raw, _ := json.Marshal(t); return authority.Digest(raw) }
func (t Trust) verifier(now time.Time) (signature.Verifier, error) {
	if t.Version != PolicyVersion || t.Scope != LocalScope || t.Repository != "os-santiago/saferus" || t.Environment != "ai-sdlc-production" ||
		!uuidRE.MatchString(t.ClusterUID) || !uuidRE.MatchString(t.BuildNamespaceUID) || t.Registry != building.Registry ||
		t.VerifierVersion != VerifierVersion || t.CompilerVersion != "ai-sdlc.saferus-build-compiler/v1" || t.PredicateType != PredicateType || t.Transparency != LocalTransparency ||
		t.ApprovedAt <= 0 || t.ApprovedAt > now.UnixMilli() || t.ExpiresAt <= now.UnixMilli() || t.ExpiresAt <= t.ApprovedAt || t.ExpiresAt-t.ApprovedAt > (24*time.Hour).Milliseconds() ||
		!strings.HasPrefix(t.VerifierImage, "quay.io/os-santiago/ai-sdlc@") || !sha256RE.MatchString(strings.TrimPrefix(t.VerifierImage, "quay.io/os-santiago/ai-sdlc@")) ||
		len(t.SignerPublicKey) == 0 || len(t.SignerPublicKey) > 4096 || len(t.RegistryCA) == 0 || len(t.RegistryCA) > 65536 {
		return nil, ErrProvenance
	}
	keyBytes := bytes.TrimSpace([]byte(t.SignerPublicKey))
	if !bytes.HasPrefix(keyBytes, []byte("-----BEGIN PUBLIC KEY-----")) {
		return nil, ErrProvenance
	}
	block, rest := pem.Decode(keyBytes)
	if block == nil || block.Type != "PUBLIC KEY" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 || authority.Digest(block.Bytes) != t.SignerSHA256 {
		return nil, ErrProvenance
	}
	key, e := x509.ParsePKIXPublicKey(block.Bytes)
	if e != nil {
		return nil, ErrProvenance
	}
	ec, ok := key.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, ErrProvenance
	}
	verifier, e := signature.LoadVerifier(key, crypto.SHA256)
	if e != nil {
		return nil, ErrProvenance
	}
	return verifier, nil
}
func (t Trust) matches(r buildrecord.Record) bool {
	return r.Validate() == nil && r.Command.Repository == t.Repository && r.Command.Environment == t.Environment &&
		r.Context.Profile.ClusterUID == t.ClusterUID && r.Context.Profile.NamespaceUID == t.BuildNamespaceUID &&
		r.Result.ImageURL == t.Registry && r.Result.PublisherImage == t.PublisherImage && r.Context.Compiler == t.CompilerVersion
}

// Validate checks independently installed public trust without credentials or I/O.
func (t Trust) Validate(now time.Time) error {
	if _, err := t.verifier(now); err != nil {
		return err
	}
	_, err := trustRoots(t.RegistryCA)
	return err
}

// ParseTrust decodes the independently mounted public trust document with the
// same closed schema as evidence. It does not approve candidate observations.
func ParseTrust(raw []byte, now time.Time) (Trust, error) {
	var out Trust
	if len(raw) > 128*1024 || strictObject(raw, &out) != nil || out.Validate(now) != nil {
		return Trust{}, ErrProvenance
	}
	return out, nil
}
