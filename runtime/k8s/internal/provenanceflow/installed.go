package provenanceflow

import (
	"io"
	"os"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
)

// Mounts are trusted installation paths, never issue/model/receipt input.
// VerifierImage is the installer-pinned deployment image, independently supplied
// from the trust document; installation must verify the actual deployment pin.
type Mounts struct {
	Build, Inner, Outer, Trust, Identity, Credential, VerifierImage string
}

func InstalledMounts(verifierImage string) Mounts {
	return Mounts{Build: "/etc/ai-sdlc/build/config.json", Inner: "/etc/ai-sdlc/authority/policy.json", Outer: "/etc/ai-sdlc/outer-policy/policy.json", Trust: "/etc/ai-sdlc/provenance/trust.json", Identity: "/etc/ai-sdlc/identity/identity.json", Credential: "/var/run/ai-sdlc/provenance-registry/credential.json", VerifierImage: verifierImage}
}

func readMounted(path string, limit int64) ([]byte, error) {
	if path == "" {
		return nil, ErrFlow
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, ErrFlow
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil || len(raw) == 0 || int64(len(raw)) > limit {
		return nil, ErrFlow
	}
	return raw, nil
}

// Load reopens current canonical policies, public trust and the component's own
// signing identity on every call. It never reads registry credentials.
func (m Mounts) Load(role string) (provenance.Configuration, authority.Identity, error) {
	var empty provenance.Configuration
	var key authority.Identity
	if role != "build-controller" && role != "verifier" {
		return empty, key, ErrFlow
	}
	i, e := buildflow.LoadCurrent(m.Build, m.Inner, m.Outer)
	if e != nil {
		return empty, key, ErrFlow
	}
	raw, e := readMounted(m.Trust, 128*1024)
	if e != nil {
		return empty, key, ErrFlow
	}
	trust, e := provenance.ParseTrust(raw, time.Now())
	if e != nil {
		return empty, key, ErrFlow
	}
	if trust.VerifierImage != m.VerifierImage || trust.ClusterUID != i.Profile.ClusterUID || trust.BuildNamespaceUID != i.Profile.NamespaceUID || trust.PublisherImage != i.Profile.PublisherImage {
		return empty, key, ErrFlow
	}
	cfg := provenance.Configuration{Build: i.Configuration(), Trust: trust}
	if cfg.Validate(time.Now()) != nil {
		return empty, key, ErrFlow
	}
	raw, e = readMounted(m.Identity, 16384)
	if e != nil || authority.DecodeStrict(raw, &key) != nil || key.Role != role || key.Validate(cfg.Build.Outer) != nil {
		return empty, authority.Identity{}, ErrFlow
	}
	return cfg, key, nil
}

// Verifier reads only the separately mounted pull-only credential. Native ACL
// conformance is an installation prerequisite; a username does not prove it.
func (m Mounts) Verifier(cfg provenance.Configuration) (EvidenceVerifier, error) {
	raw, e := readMounted(m.Credential, 1024)
	if e != nil {
		return nil, ErrFlow
	}
	var credential struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if authority.DecodeStrict(raw, &credential) != nil {
		return nil, ErrFlow
	}
	if cfg.Validate(time.Now()) != nil || cfg.Trust.VerifierImage != m.VerifierImage {
		return nil, ErrFlow
	}
	return provenance.NewVerifier(cfg.Trust, provenance.Credential{Username: credential.Username, Password: credential.Password})
}
