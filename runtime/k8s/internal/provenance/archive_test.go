package provenance

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildrecord"
	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/sigstore/sigstore/pkg/signature/dsse"
)

// This archive is from an actual, subsequently removed Tekton workload. It
// demonstrates format compatibility; it is not a current authenticated record.
func TestArchivedTektonChainsSignatureAndClosedBinding(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		raw, err := os.ReadFile("testdata/chains-v02/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	block, _ := pem.Decode(read("cosign.pub"))
	if block == nil {
		t.Fatal("fixture public key")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := signature.LoadVerifier(key, crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := dsse.WrapVerifier(verifier, dsse.WithExpectedPayloadType("application/vnd.in-toto+json"))
	raw := read("attestation.json")
	if err = wrapped.VerifySignature(bytes.NewReader(raw), nil); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Payload string `json:"payload"`
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(envelope.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var record buildrecord.Record
	if err = json.Unmarshal(read("binding.json"), &record); err != nil {
		t.Fatal(err)
	}
	if record.Validate() == nil {
		t.Fatal("partial archive must not become authenticated history")
	}
	if err = bindStatement(payload, record); err != nil {
		t.Fatal("actual Chains format rejected", err)
	}
	record.Command.SourceSHA = "0000000000000000000000000000000000000000"
	if bindStatement(payload, record) == nil {
		t.Fatal("archived signature accepted for different source")
	}
}
