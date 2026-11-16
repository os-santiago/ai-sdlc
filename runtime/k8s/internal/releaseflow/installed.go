package releaseflow

import (
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"io"
	"os"
	"time"
)

// Mounts are reviewed installation inputs. No request chooses paths or targets.
type Mounts struct{ Build, Inner, Outer, Trust, Target, Identity, VerifierImage string }

func InstalledMounts(verifierImage string) Mounts {
	return Mounts{Build: "/etc/ai-sdlc/build/config.json", Inner: "/etc/ai-sdlc/authority/policy.json", Outer: "/etc/ai-sdlc/outer-policy/policy.json", Trust: "/etc/ai-sdlc/provenance/trust.json", Target: "/etc/ai-sdlc/release/target.json", Identity: "/etc/ai-sdlc/identity/identity.json", VerifierImage: verifierImage}
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

// LoadTarget is intentionally independent of advance policy. Only the journal
// may combine it with an original acknowledged effect for bounded containment.
// Missing/replaced/expired destination never selects cached or request input.
func (m Mounts) LoadTarget() (releasing.Target, error) {
	var target releasing.Target
	raw, e := readMounted(m.Target, 96*1024)
	if e != nil || authority.DecodeStrict(raw, &target) != nil || target.Validate(time.Now()) != nil {
		return releasing.Target{}, ErrFlow
	}
	return target, nil
}
func (m Mounts) Load(role string) (releasing.Configuration, authority.Identity, error) {
	var cfg releasing.Configuration
	var key authority.Identity
	if role != "release-manager" && role != "verifier" && role != "deployer" {
		return cfg, key, ErrFlow
	}
	installed, e := buildflow.LoadCurrent(m.Build, m.Inner, m.Outer)
	if e != nil {
		return cfg, key, ErrFlow
	}
	raw, e := readMounted(m.Trust, 128*1024)
	if e != nil {
		return cfg, key, ErrFlow
	}
	trust, e := provenance.ParseTrust(raw, time.Now())
	if e != nil || trust.VerifierImage != m.VerifierImage || trust.ClusterUID != installed.Profile.ClusterUID || trust.BuildNamespaceUID != installed.Profile.NamespaceUID || trust.PublisherImage != installed.Profile.PublisherImage {
		return cfg, key, ErrFlow
	}
	target, e := m.LoadTarget()
	if e != nil {
		return cfg, key, e
	}
	cfg = releasing.Configuration{Provenance: provenance.Configuration{Build: installed.Configuration(), Trust: trust}, Target: target}
	if cfg.Validate(time.Now()) != nil {
		return releasing.Configuration{}, key, ErrFlow
	}
	raw, e = readMounted(m.Identity, 16384)
	if e != nil || authority.DecodeStrict(raw, &key) != nil || key.Role != role || key.Validate(cfg.Provenance.Build.Outer) != nil {
		return releasing.Configuration{}, authority.Identity{}, ErrFlow
	}
	return cfg, key, nil
}
