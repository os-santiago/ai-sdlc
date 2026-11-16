package provenance

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildrecord"
	"golang.org/x/crypto/bcrypt"
)

// This fixture emits synthetic compiled history/signatures for a disposable
// native registry. It does not create operational trust or live build records.
func TestEmitNativeZotProvenanceFixture(t *testing.T) {
	root := os.Getenv("AI_SDLC_NATIVE_ZOT_FIXTURE")
	if root == "" {
		t.Skip("owned disposable native registry fixture required")
	}
	write := func(name string, data []byte) {
		t.Helper()
		f, e := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.Write(data); e != nil {
			f.Close()
			t.Fatal(e)
		}
		if e = f.Close(); e != nil {
			t.Fatal(e)
		}
	}
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	content := []byte("isolated registry protocol fixture\n")
	if e := tw.WriteHeader(&tar.Header{Name: "fixture.txt", Mode: 0444, Size: int64(len(content))}); e != nil {
		t.Fatal(e)
	}
	if _, e := tw.Write(content); e != nil {
		t.Fatal(e)
	}
	if e := tw.Close(); e != nil {
		t.Fatal(e)
	}
	config := jsonBytes(t, map[string]any{"architecture": "amd64", "os": "linux", "rootfs": map[string]any{"type": "layers", "diff_ids": []string{digest(layer.Bytes())}}})
	artifact := jsonBytes(t, manifest{SchemaVersion: 2, MediaType: ociManifest, Config: descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: digest(config), Size: int64(len(config))}, Layers: []descriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: digest(layer.Bytes()), Size: int64(layer.Len())}}})
	f := newFixture(t, artifact)
	cert, ca := tlsFixture(t)
	f.trust.RegistryCA = ca
	key, e := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if e != nil {
		t.Fatal(e)
	}
	write("ca.crt", []byte(ca))
	write("tls.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}))
	write("tls.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
	credentials := map[string]string{}
	var hashes bytes.Buffer
	for _, user := range []string{"ai-sdlc-publisher", "ai-sdlc-verifier", "fixture-admin"} {
		raw := make([]byte, 32)
		if _, e = rand.Read(raw); e != nil {
			t.Fatal(e)
		}
		password := hex.EncodeToString(raw)
		credentials[user] = password
		hash, e := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if e != nil {
			t.Fatal(e)
		}
		hashes.WriteString(user + ":" + string(hash) + "\n")
	}
	write("credentials.json", jsonBytes(t, credentials))
	write("htpasswd", hashes.Bytes())
	aclRaw, e := os.ReadFile("../../deploy/konflux/provenance-registry-access.json")
	if e != nil {
		t.Fatal(e)
	}
	var acl map[string]any
	if json.Unmarshal(aclRaw, &acl) != nil {
		t.Fatal("ACL blueprint")
	}
	// Administrator exists only in this disposable fixture to seed a known other
	// repository. It is absent from the installation policy blueprint.
	acl["adminPolicy"] = map[string]any{"users": []string{"fixture-admin"}, "actions": []string{"read", "create", "update", "delete"}}
	native := map[string]any{"distSpecVersion": "1.1.1", "storage": map[string]string{"rootDirectory": "/var/lib/zot"}, "http": map[string]any{"address": "0.0.0.0", "port": "5000", "tls": map[string]string{"cert": "/cfg/tls.crt", "key": "/cfg/tls.key"}, "auth": map[string]any{"htpasswd": map[string]string{"path": "/cfg/htpasswd"}}, "accessControl": acl}, "log": map[string]string{"level": "error"}}
	write("config.json", jsonBytes(t, native))
	f.routes[registryPath+"blobs/"+digest(config)] = config
	f.routes[registryPath+"blobs/"+digest(layer.Bytes())] = layer.Bytes()
	f.routes[registryPath+"blobs/"+digest([]byte("{}"))] = []byte("{}")
	write("routes.json", jsonBytes(t, f.routes))
	write("record.json", jsonBytes(t, f.record))
	write("trust.json", jsonBytes(t, f.trust))
}

func TestActualNativeZotRetainedProvenance(t *testing.T) {
	root := os.Getenv("AI_SDLC_NATIVE_ZOT_FIXTURE")
	address := os.Getenv("AI_SDLC_NATIVE_ZOT_ADDRESS")
	if root == "" || address == "" {
		t.Skip("owned disposable native registry required")
	}
	host, port, e := net.SplitHostPort(address)
	if e != nil || host != "127.0.0.1" || port == "" {
		t.Fatal("invalid owned endpoint")
	}
	read := func(name string, value any) {
		t.Helper()
		raw, e := os.ReadFile(filepath.Join(root, name))
		if e != nil || json.Unmarshal(raw, value) != nil {
			t.Fatal("invalid native fixture input")
		}
	}
	var record buildrecord.Record
	var trust Trust
	var credentials map[string]string
	read("record.json", &record)
	read("trust.json", &trust)
	read("credentials.json", &credentials)
	v, e := NewVerifier(trust, Credential{"ai-sdlc-verifier", credentials["ai-sdlc-verifier"]})
	if e != nil {
		t.Fatal(e)
	}
	v.registry.client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, target string) (net.Conn, error) {
		if target != registryHost+":443" {
			return nil, ErrProvenance
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
	}
	proof, e := v.Verify(context.Background(), record)
	if e != nil || proof.ReleaseAuthorized || proof.BuildRecordSHA256 != record.Digest() || v.registry.requests != 5 {
		t.Fatal("native registry provenance verification", e)
	}
	raw := jsonBytes(t, proof)
	if e = os.WriteFile(filepath.Join(root, "verified-proof.json"), raw, 0600); e != nil {
		t.Fatal(e)
	}
}
