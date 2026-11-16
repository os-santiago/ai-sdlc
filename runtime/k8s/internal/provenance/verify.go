package provenance

import (
	"bytes"
	"context"
	"encoding/base64"
	"sync/atomic"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildrecord"
	"github.com/sigstore/sigstore/pkg/signature/dsse"
)

type Proof struct {
	Version                   string `json:"version"`
	Scope                     string `json:"scope"`
	BuildRecordSHA256         string `json:"buildRecordSHA256"`
	RetainedContextSHA256     string `json:"retainedContextSHA256"`
	TrustSHA256               string `json:"trustSHA256"`
	VerifierImage             string `json:"verifierImage"`
	VerifierVersion           string `json:"verifierVersion"`
	ImageDigest               string `json:"imageDigest"`
	SignatureManifestDigest   string `json:"signatureManifestDigest"`
	SignatureBlobDigest       string `json:"signatureBlobDigest"`
	AttestationManifestDigest string `json:"attestationManifestDigest"`
	AttestationBlobDigest     string `json:"attestationBlobDigest"`
	BuildObservationSHA256    string `json:"buildObservationSHA256"`
	PublishObservationSHA256  string `json:"publishObservationSHA256"`
	Transparency              string `json:"transparency"`
	ReleaseAuthorized         bool   `json:"releaseAuthorized"`
}
type Verifier struct {
	used     atomic.Bool
	trust    Trust
	registry *registry
}

func NewVerifier(t Trust, c Credential) (*Verifier, error) {
	if _, e := t.verifier(time.Now()); e != nil {
		return nil, e
	}
	r, e := newRegistry(t, c)
	if e != nil {
		return nil, e
	}
	return &Verifier{trust: t, registry: r}, nil
}

// Verify consumes authenticated history, but its return value grants no current
// authority. The owning journal must recheck installed policy before committing.
// A verifier instance is single-use, with five GETs and one total deadline.
func (v *Verifier) Verify(ctx context.Context, r buildrecord.Record) (Proof, error) {
	if v == nil || v.registry == nil || !v.used.CompareAndSwap(false, true) || ctx.Err() != nil || !v.trust.matches(r) {
		return Proof{}, ErrProvenance
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	defer v.registry.client.CloseIdleConnections()
	key, e := v.trust.verifier(time.Now())
	if e != nil {
		return Proof{}, e
	}
	if _, e = v.registry.artifact(ctx, r.Result.ImageDigest); e != nil {
		return Proof{}, e
	}
	sig, e := v.registry.evidence(ctx, r.Result.ImageDigest, "sig", signatureMedia)
	if e != nil {
		return Proof{}, e
	}
	encoded := sig.Annotations["dev.cosignproject.cosign/signature"]
	if len(encoded) == 0 || len(encoded) > 1024 {
		return Proof{}, ErrProvenance
	}
	signature, e := base64.StdEncoding.Strict().DecodeString(encoded)
	if e != nil || len(signature) > 256 || key.VerifySignature(bytes.NewReader(signature), bytes.NewReader(sig.Data)) != nil || bindImage(sig.Data, r) != nil {
		return Proof{}, ErrProvenance
	}
	att, e := v.registry.evidence(ctx, r.Result.ImageDigest, "att", attestationMedia)
	if e != nil {
		return Proof{}, e
	}
	var envelope struct {
		PayloadType string `json:"payloadType"`
		Payload     string `json:"payload"`
		Signatures  []struct {
			KeyID string `json:"keyid"`
			Sig   string `json:"sig"`
		} `json:"signatures"`
	}
	if strictObject(att.Data, &envelope) != nil || envelope.PayloadType != "application/vnd.in-toto+json" || len(envelope.Signatures) != 1 || len(envelope.Signatures[0].Sig) == 0 || len(envelope.Signatures[0].Sig) > 1024 {
		return Proof{}, ErrProvenance
	}
	payload, e := base64.StdEncoding.Strict().DecodeString(envelope.Payload)
	if e != nil || len(payload) > MaxEvidenceBytes {
		return Proof{}, ErrProvenance
	}
	wrapped := dsse.WrapVerifier(key, dsse.WithExpectedPayloadType("application/vnd.in-toto+json"))
	if wrapped.VerifySignature(bytes.NewReader(att.Data), nil) != nil || bindStatement(payload, r) != nil || ctx.Err() != nil {
		return Proof{}, ErrProvenance
	}
	if _, e = v.trust.verifier(time.Now()); e != nil {
		return Proof{}, e
	}
	contextRaw, e := r.Context.Encode()
	if e != nil {
		return Proof{}, ErrProvenance
	}
	return Proof{Version: PolicyVersion, Scope: LocalScope, BuildRecordSHA256: r.Digest(), RetainedContextSHA256: authority.Digest(contextRaw), TrustSHA256: v.trust.Digest(), VerifierImage: v.trust.VerifierImage, VerifierVersion: VerifierVersion, ImageDigest: r.Result.ImageDigest, SignatureManifestDigest: sig.ManifestDigest, SignatureBlobDigest: sig.BlobDigest, AttestationManifestDigest: att.ManifestDigest, AttestationBlobDigest: att.BlobDigest, BuildObservationSHA256: r.Build.Digest(), PublishObservationSHA256: r.Publish.Digest(), Transparency: LocalTransparency, ReleaseAuthorized: false}, nil
}
