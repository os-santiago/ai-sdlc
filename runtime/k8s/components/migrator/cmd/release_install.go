package main

import (
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
)

// Offline validation of separately approved target plus current public objects.
// No approval, Kubernetes access, credential input, write or workflow authority.
func compileReleaseInstallation(input io.Reader, output io.Writer) error {
	denied := errors.New("release installation input denied")
	raw, e := io.ReadAll(io.LimitReader(input, 512*1024+1))
	if e != nil || len(raw) > 512*1024 {
		return denied
	}
	var req struct {
		Target     json.RawMessage `json:"target"`
		Trust      json.RawMessage `json:"trust"`
		Deployment apps.Deployment `json:"deployment"`
		Service    core.Service    `json:"service"`
	}
	if !exactInstallFields(raw, []string{"target", "trust", "deployment", "service"}, nil) || authority.DecodeStrict(raw, &req) != nil {
		return denied
	}
	var target releasing.Target
	if authority.DecodeStrict(req.Target, &target) != nil || !canonicalInstallValue(req.Target, target) || target.Validate(time.Now()) != nil {
		return denied
	}
	trust, e := provenance.ParseTrust(req.Trust, time.Now())
	if e != nil || trust.ClusterUID != target.ClusterUID || trust.Repository != target.Repository || trust.Environment != target.Environment {
		return denied
	}
	d, s := req.Deployment, req.Service
	if d.APIVersion != "apps/v1" || d.Kind != "Deployment" || s.APIVersion != "v1" || s.Kind != "Service" || d.Name != target.Deployment || s.Name != target.Service || d.Namespace != target.Namespace || s.Namespace != target.Namespace || string(d.UID) != target.DeploymentUID || string(s.UID) != target.ServiceUID || d.DeletionTimestamp != nil || s.DeletionTimestamp != nil || len(d.OwnerReferences) != 0 || len(s.OwnerReferences) != 0 || len(d.Finalizers) != 0 || len(s.Finalizers) != 0 {
		return denied
	}
	dh, e := podtransport.ConsoleSpecDigest(d.Spec)
	if e != nil || dh != target.DeploymentSpecSHA256 {
		return denied
	}
	sh, e := podtransport.ConsoleServiceDigest(s.Spec)
	if e != nil || sh != target.ServiceSpecSHA256 {
		return denied
	}
	if d.Spec.Template.Spec.Containers[0].Image != target.BaselineImage || len(d.Spec.Template.Spec.ImagePullSecrets) != 1 || d.Spec.Template.Spec.ImagePullSecrets[0].Name != target.PullSecret {
		return denied
	}
	return json.NewEncoder(output).Encode(struct {
		Version      string           `json:"version"`
		Target       releasing.Target `json:"target"`
		TargetSHA256 string           `json:"targetSHA256"`
		TrustSHA256  string           `json:"trustSHA256"`
	}{"ai-sdlc.release-installation/v1", target, target.Digest(), trust.Digest()})
}

// describeReleaseSpecs emits public canonical observations for independent
// review. It grants no target approval and has no Kubernetes or DB client.
func describeReleaseSpecs(input io.Reader, output io.Writer) error {
	denied := errors.New("release observation input denied")
	raw, e := io.ReadAll(io.LimitReader(input, 512*1024+1))
	if e != nil || len(raw) > 512*1024 {
		return denied
	}
	var req struct {
		Deployment apps.Deployment `json:"deployment"`
		Service    core.Service    `json:"service"`
	}
	if !exactInstallFields(raw, []string{"deployment", "service"}, nil) || authority.DecodeStrict(raw, &req) != nil {
		return denied
	}
	d, s := req.Deployment, req.Service
	if d.APIVersion != "apps/v1" || d.Kind != "Deployment" || s.APIVersion != "v1" || s.Kind != "Service" || d.Name != "saferus-console" || s.Name != d.Name || d.Namespace != "ai-sdlc-production" || s.Namespace != d.Namespace || d.DeletionTimestamp != nil || s.DeletionTimestamp != nil {
		return denied
	}
	dh, e := podtransport.ConsoleSpecDigest(d.Spec)
	if e != nil {
		return denied
	}
	sh, e := podtransport.ConsoleServiceDigest(s.Spec)
	if e != nil {
		return denied
	}
	return json.NewEncoder(output).Encode(struct {
		Version              string `json:"version"`
		DeploymentSpecSHA256 string `json:"deploymentSpecSHA256"`
		ServiceSpecSHA256    string `json:"serviceSpecSHA256"`
		BaselineImage        string `json:"baselineImage"`
	}{"ai-sdlc.release-specs/v1", dh, sh, d.Spec.Template.Spec.Containers[0].Image})
}
