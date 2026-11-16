// Package releasing defines fresh, bounded authority for the installed local
// Saferus console. It does not execute Kubernetes requests or grant DB access.
package releasing

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	"regexp"
	"strings"
	"time"
)

const TargetVersion = "ai-sdlc.saferus-release-target/v1"

var ErrRelease = errors.New("fresh release authority denied")
var uuid = regexp.MustCompile(`^[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}$`)
var imageDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Target is independently approved installation input. Hashes cover the full
// Deployment spec with only container image normalized, and the full Service
// spec. The operational adapter must re-observe these before every effect.
// This first recipe supports exactly one console container in kind test prod.
type Target struct {
	Version               string `json:"version"`
	Scope                 string `json:"scope"`
	Repository            string `json:"repository"`
	Environment           string `json:"environment"`
	ClusterUID            string `json:"clusterUID"`
	NamespaceUID          string `json:"namespaceUID"`
	Namespace             string `json:"namespace"`
	Deployment            string `json:"deployment"`
	DeploymentUID         string `json:"deploymentUID"`
	Container             string `json:"container"`
	DeploymentSpecSHA256  string `json:"deploymentSpecSHA256"`
	Service               string `json:"service"`
	ServiceUID            string `json:"serviceUID"`
	ServiceSpecSHA256     string `json:"serviceSpecSHA256"`
	PullSecret            string `json:"pullSecret"`
	PullSecretUID         string `json:"pullSecretUID"`
	TLSSecretUID          string `json:"tlsSecretUID"`
	TLSCA                 string `json:"tlsCA"`
	TLSHostname           string `json:"tlsHostname"`
	BaselineImage         string `json:"baselineImage"`
	BaselineRuntimeDigest string `json:"baselineRuntimeDigest"`
	RolloutSeconds        int    `json:"rolloutSeconds"`
	SmokeSeconds          int    `json:"smokeSeconds"`
	ApprovedAt            int64  `json:"approvedAt"`
	ExpiresAt             int64  `json:"expiresAt"`
}

func (t Target) Digest() string { b, _ := json.Marshal(t); return authority.Digest(b) }
func (t Target) Validate(now time.Time) error {
	if t.Version != TargetVersion || t.Scope != "kind-test-production" || t.Repository != "os-santiago/saferus" || t.Environment != "ai-sdlc-production" || t.Namespace != t.Environment || t.Deployment != "saferus-console" || t.Container != "console" || t.Service != "saferus-console" || t.PullSecret != "ai-sdlc-release-reader" || t.TLSHostname != "saferus-console" || t.RolloutSeconds != 300 || t.SmokeSeconds != 30 || t.ApprovedAt <= 0 || t.ApprovedAt > now.UnixMilli() || t.ExpiresAt <= now.UnixMilli() || t.ExpiresAt-t.ApprovedAt > (24*time.Hour).Milliseconds() {
		return ErrRelease
	}
	seen := map[string]bool{}
	for _, id := range []string{t.ClusterUID, t.NamespaceUID, t.DeploymentUID, t.ServiceUID, t.PullSecretUID, t.TLSSecretUID} {
		if !uuid.MatchString(id) || seen[id] {
			return ErrRelease
		}
		seen[id] = true
	}
	for _, d := range []string{t.DeploymentSpecSHA256, t.ServiceSpecSHA256} {
		if !workspacebundle.ValidDigest(d) {
			return ErrRelease
		}
	}
	// Only the installed initial image or this recipe's retained registry is a
	// permitted baseline; tags and arbitrary registries cannot enter rollback.
	validImage := false
	for _, prefix := range []string{"localhost/saferus-console@", building.Registry + "@"} {
		if strings.HasPrefix(t.BaselineImage, prefix) && imageDigest.MatchString(strings.TrimPrefix(t.BaselineImage, prefix)) {
			validImage = true
		}
	}
	if !imageDigest.MatchString(t.BaselineRuntimeDigest) || !validImage || len(t.TLSCA) == 0 || len(t.TLSCA) > 65536 {
		return ErrRelease
	}
	raw := bytes.TrimSpace([]byte(t.TLSCA))
	if !bytes.HasPrefix(raw, []byte("-----BEGIN CERTIFICATE-----")) {
		return ErrRelease
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return ErrRelease
	}
	cert, e := x509.ParseCertificate(block.Bytes)
	if e != nil || !cert.IsCA || !cert.BasicConstraintsValid || cert.KeyUsage&x509.KeyUsageCertSign == 0 || now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) || time.UnixMilli(t.ExpiresAt).After(cert.NotAfter) {
		return ErrRelease
	}
	return nil
}
