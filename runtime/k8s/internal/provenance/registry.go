package provenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

const registryHost = "registry-service.kind-registry"
const registryPath = "/v2/ai-sdlc-fixtures/saferus/"
const manifestLimit = 256 << 10
const ociManifest = "application/vnd.oci.image.manifest.v1+json"
const dockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
const signatureMedia = "application/vnd.dev.cosign.simplesigning.v1+json"
const attestationMedia = "application/vnd.dsse.envelope.v1+json"

// Credential belongs to the independently provisioned read-only registry user.
// Installation must prove ACL denials; a username alone cannot establish them.
type Credential struct{ Username, Password string }
type registry struct {
	client     *http.Client
	credential Credential
	requests   int
}
type descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	URLs         []string          `json:"urls,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Data         string            `json:"data,omitempty"`
}
type manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        descriptor        `json:"config"`
	Layers        []descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Subject       *descriptor       `json:"subject,omitempty"`
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func newRegistry(t Trust, c Credential) (*registry, error) {
	if c.Username != "ai-sdlc-verifier" || len(c.Password) < 32 || len(c.Password) > 256 || strings.ContainsAny(c.Password, "\r\n\x00") {
		return nil, ErrProvenance
	}
	roots := x509.NewCertPool()
	remaining := []byte(t.RegistryCA)
	count := 0
	for len(bytes.TrimSpace(remaining)) > 0 {
		remaining = bytes.TrimSpace(remaining)
		if !bytes.HasPrefix(remaining, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, ErrProvenance
		}
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, ErrProvenance
		}
		cert, e := x509.ParseCertificate(block.Bytes)
		if e != nil || !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 {
			return nil, ErrProvenance
		}
		roots.AddCert(cert)
		count++
		if count > 4 {
			return nil, ErrProvenance
		}
		remaining = rest
	}
	if count == 0 {
		return nil, ErrProvenance
	}
	transport := &http.Transport{Proxy: nil, DisableCompression: true, DisableKeepAlives: true,
		DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
		MaxResponseHeaderBytes: 16384, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: registryHost}}
	return &registry{client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrProvenance }}, credential: c}, nil
}
func (r *registry) get(ctx context.Context, route string, limit int) ([]byte, error) {
	if r.requests >= 5 {
		return nil, ErrProvenance
	}
	r.requests++
	request, e := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+registryHost+registryPath+route, nil)
	if e != nil {
		return nil, ErrProvenance
	}
	request.Header.Set("Accept", ociManifest+", "+dockerManifest+", application/octet-stream")
	request.Header.Set("Accept-Encoding", "identity")
	request.SetBasicAuth(r.credential.Username, r.credential.Password)
	response, e := r.client.Do(request)
	if e != nil {
		return nil, ErrProvenance
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > int64(limit) || (response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity") {
		return nil, ErrProvenance
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if e != nil || len(raw) == 0 || len(raw) > limit {
		return nil, ErrProvenance
	}
	return raw, nil
}
func (r *registry) artifact(ctx context.Context, imageDigest string) (string, error) {
	if !sha256RE.MatchString(imageDigest) {
		return "", ErrProvenance
	}
	raw, e := r.get(ctx, "manifests/"+imageDigest, manifestLimit)
	if e != nil || digest(raw) != imageDigest {
		return "", ErrProvenance
	}
	var m manifest
	if strictObject(raw, &m) != nil || m.SchemaVersion != 2 || (m.MediaType != ociManifest && m.MediaType != dockerManifest) {
		return "", ErrProvenance
	}
	if m.ArtifactType != "" || m.Subject != nil || len(m.Layers) != 1 || !closedDescriptor(m.Config, 65536) || (m.Config.MediaType != "application/vnd.oci.image.config.v1+json" && m.Config.MediaType != "application/vnd.docker.container.image.v1+json") {
		return "", ErrProvenance
	}
	l := m.Layers[0]
	if !closedDescriptor(l, 64<<20) || (l.MediaType != "application/vnd.oci.image.layer.v1.tar" && l.MediaType != "application/vnd.oci.image.layer.v1.tar+gzip" && l.MediaType != "application/vnd.docker.image.rootfs.diff.tar.gzip") {
		return "", ErrProvenance
	}
	return digest(raw), nil
}

type layerEvidence struct {
	ManifestDigest, BlobDigest string
	Data                       []byte
	Annotations                map[string]string
}

func (r *registry) evidence(ctx context.Context, imageDigest, suffix, media string) (layerEvidence, error) {
	if !sha256RE.MatchString(imageDigest) || (suffix != "sig" && suffix != "att") {
		return layerEvidence{}, ErrProvenance
	}
	tag := "sha256-" + strings.TrimPrefix(imageDigest, "sha256:") + "." + suffix
	raw, e := r.get(ctx, "manifests/"+tag, manifestLimit)
	if e != nil {
		return layerEvidence{}, e
	}
	var m manifest
	if strictObject(raw, &m) != nil || m.SchemaVersion != 2 || (m.MediaType != ociManifest && m.MediaType != dockerManifest) || len(m.Layers) != 1 || m.ArtifactType != "" || m.Subject != nil || !closedDescriptor(m.Config, 65536) {
		return layerEvidence{}, ErrProvenance
	}
	l := m.Layers[0]
	if l.MediaType != media || !closedDescriptor(l, MaxEvidenceBytes) {
		return layerEvidence{}, ErrProvenance
	}
	blob, e := r.get(ctx, "blobs/"+l.Digest, MaxEvidenceBytes)
	if e != nil || int64(len(blob)) != l.Size || digest(blob) != l.Digest {
		return layerEvidence{}, ErrProvenance
	}
	return layerEvidence{digest(raw), l.Digest, blob, l.Annotations}, nil
}

// strictObject rejects duplicate keys, unknown fields and case aliases. Raw
// metadata remains bounded and cannot supply an expectation or permission.
func strictObject(raw []byte, v any) error {
	if len(raw) == 0 || len(raw) > MaxEvidenceBytes || authority.DecodeStrict(raw, v) != nil {
		return ErrProvenance
	}
	canonical, e := json.Marshal(v)
	if e != nil {
		return ErrProvenance
	}
	var original, normalized any
	if json.Unmarshal(raw, &original) != nil || json.Unmarshal(canonical, &normalized) != nil || !canonicalKeys(original, normalized) {
		return ErrProvenance
	}
	return nil
}
func canonicalKeys(original, normalized any) bool {
	switch a := original.(type) {
	case map[string]any:
		b, ok := normalized.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range a {
			n, ok := b[k]
			if !ok || !canonicalKeys(v, n) {
				return false
			}
		}
	case []any:
		b, ok := normalized.([]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i, v := range a {
			if !canonicalKeys(v, b[i]) {
				return false
			}
		}
	}
	return true
}

func closedDescriptor(d descriptor, limit int64) bool {
	return sha256RE.MatchString(d.Digest) && d.Size > 0 && d.Size <= limit && len(d.URLs) == 0 && d.Data == "" && d.ArtifactType == ""
}
