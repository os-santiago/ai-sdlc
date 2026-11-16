// Package buildworkload compiles one reviewed Saferus recipe. It performs no
// Kubernetes calls and provides no execution authority outside the build journal.
package buildworkload

import (
	"encoding/json"
	"errors"
	"net/netip"
	"regexp"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

const GoImage = "docker.io/library/golang@sha256:5e8afd493a7124a8586e134d9b532731161d4b9becf9dee5a5956b107426f93d"
const EntrypointImage = "ghcr.io/tektoncd/pipeline/entrypoint-bff0a22da108bc2f16c818c97641a296:v1.15.0@sha256:1ae5944a51f5c5f19e575de5abf268ea7a49a3a54bdad411cf27e3142af5f5c0"
const ScriptImage = "cgr.dev/chainguard/busybox@sha256:19f02276bf8dbdd62f069b922f10c65262cc34b710eea26ff928129a736be791"
const Namespace = "ai-sdlc-builds"

var ErrWorkload = errors.New("closed Saferus build workload denied")
var image = regexp.MustCompile(`^(quay\.io/os-santiago/ai-sdlc|localhost/ai-sdlc-artifact-builder)@sha256:[a-f0-9]{64}$`)
var uuid = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var required = []string{"configMap/build-trust", "mutatingAdmissionPolicy/ai-sdlc-build-bounds", "mutatingAdmissionPolicyBinding/ai-sdlc-build-bounds", "limitRange/build-budget", "serviceAccount/build", "serviceAccount/publish", "secret/registry-auth", "configMap/trusted-ca", "networkPolicy/build", "networkPolicy/publish", "validatingAdmissionPolicy/ai-sdlc-build-pods", "validatingAdmissionPolicyBinding/ai-sdlc-build-pods", "configMap/tekton-flags", "default/service/kubernetes", "default/endpoints/kubernetes", "kind-registry/service/registry-service", "kind-registry/endpoints/registry-service"}

type Dependency struct {
	UID        string `json:"uid"`
	SpecSHA256 string `json:"specSHA256"`
}
type DeniedTarget struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Profile is installed configuration. Dependencies must be re-observed by the
// transport; syntactically valid identifiers are not proof they exist or match.
type Profile struct {
	Version        string                `json:"version"`
	ClusterUID     string                `json:"clusterUID"`
	NamespaceUID   string                `json:"namespaceUID"`
	PublisherImage string                `json:"publisherImage"`
	Dependencies   map[string]Dependency `json:"dependencies"`
	DeniedTargets  []DeniedTarget        `json:"deniedTargets"`
}

func (p Profile) Digest() string { raw, _ := json.Marshal(p); return authority.Digest(raw) }
func (p Profile) Validate() error {
	if p.Version != "ai-sdlc.saferus-build-profile/v1" || p.admissionConfiguration().Validate() != nil || len(p.Dependencies) != len(required) {
		return ErrWorkload
	}
	for _, key := range required {
		d, ok := p.Dependencies[key]
		if !ok || !uuid.MatchString(d.UID) || !workspacebundle.ValidDigest(d.SpecSHA256) {
			return ErrWorkload
		}
	}
	return nil
}

// AdmissionConfiguration contains only the finite facts needed before admission
// resources exist. It is not an operational Profile and grants no build authority.
type AdmissionConfiguration struct {
	Version        string         `json:"version"`
	ClusterUID     string         `json:"clusterUID"`
	NamespaceUID   string         `json:"namespaceUID"`
	PublisherImage string         `json:"publisherImage"`
	DeniedTargets  []DeniedTarget `json:"deniedTargets"`
}

func (p Profile) admissionConfiguration() AdmissionConfiguration {
	return AdmissionConfiguration{"ai-sdlc.build-admission-input/v1", p.ClusterUID, p.NamespaceUID, p.PublisherImage, p.DeniedTargets}
}
func (c AdmissionConfiguration) Validate() error {
	if c.Version != "ai-sdlc.build-admission-input/v1" || !uuid.MatchString(c.ClusterUID) || !uuid.MatchString(c.NamespaceUID) || !image.MatchString(c.PublisherImage) || len(c.DeniedTargets) < 2 || len(c.DeniedTargets) > 8 {
		return ErrWorkload
	}
	seen := map[DeniedTarget]bool{}
	for _, t := range c.DeniedTargets {
		ip, e := netip.ParseAddr(t.Host)
		if e != nil || !ip.Is4() || !ip.IsPrivate() || ip.String() != t.Host || t.Port < 1 || t.Port > 65535 || seen[t] {
			return ErrWorkload
		}
		seen[t] = true
	}
	return nil
}

// DependencyKeys returns the finite installed inventory expected by this version.
func DependencyKeys() []string { return append([]string(nil), required...) }
