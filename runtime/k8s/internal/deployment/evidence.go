package deployment

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"net"
	"regexp"
	"strings"
	"time"
)

var uid = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var node = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// ValidateReceipt is structural only. The journal authenticates intent, one-use
// send fence, exact claim and original installed target before accepting facts.
func ValidateReceipt(t releasing.Target, p releasing.Plan, before releasing.Snapshot, hash string, rollback bool, r releasing.Receipt, sent, now time.Time) error {
	image := p.Image
	if rollback {
		image = t.BaselineImage
	}
	raw, e := CompilePatch(t, before, image, sent)
	if e != nil || r.Before != before || r.Rollback != rollback || r.RequestSHA256 != hash || hash != authority.Digest(raw) || r.After.DeploymentUID != before.DeploymentUID || r.After.TargetSHA256 != before.TargetSHA256 || r.After.Generation != before.Generation+1 || !resourceVersion.MatchString(r.After.ResourceVersion) || r.After.ResourceVersion == before.ResourceVersion || r.After.Image != image || r.After.ObservedAt < before.ObservedAt || r.After.ObservedAt < sent.UnixMilli() || r.After.ObservedAt > now.Add(5*time.Second).UnixMilli() || r.After.ObservedAt >= p.ExpiresAt {
		return ErrDeployment
	}
	return nil
}
func ValidateHealth(t releasing.Target, r releasing.Receipt, h releasing.Health, now time.Time) error {
	p := h.Rollout
	parts := strings.Split(r.After.Image, "@")
	if len(parts) != 2 {
		return ErrDeployment
	}
	expected := parts[1]
	if r.Rollback {
		expected = t.BaselineRuntimeDigest
	}
	if h.Version != "ai-sdlc.saferus-health/v1" || p.DeploymentUID != t.DeploymentUID || p.Generation != r.After.Generation || p.Image != r.After.Image || p.TargetSHA256 != t.Digest() || p.ServiceUID != t.ServiceUID || strings.LastIndex(p.ImageID, "@") < 0 || p.ImageID[strings.LastIndex(p.ImageID, "@")+1:] != expected || len(h.ResponseSHA256) != 3 || h.ObservedAt < p.ObservedAt || p.ObservedAt < r.After.ObservedAt || h.ObservedAt > now.Add(5*time.Second).UnixMilli() || h.ObservedAt < now.Add(-45*time.Second).UnixMilli() || !node.MatchString(p.Node) {
		return ErrDeployment
	}
	for _, v := range []string{p.ReplicaSetUID, p.PodUID, p.EndpointSliceUID} {
		if !uid.MatchString(v) {
			return ErrDeployment
		}
	}
	for _, v := range []string{p.PodIP, p.ServiceIP} {
		ip := net.ParseIP(v)
		if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
			return ErrDeployment
		}
	}
	for _, v := range h.ResponseSHA256 {
		if !digest.MatchString(v) {
			return ErrDeployment
		}
	}
	return nil
}

// ValidateBrowserHealth checks the observation's closed contract. Only the
// store history reader authenticates intent/send, original Pod and cleanup.
func ValidateBrowserHealth(p browsercheck.Profile, t releasing.Target, receipt releasing.Receipt, h releasing.Health) error {
	raw, _ := json.Marshal(receipt.After)
	e := h.Browser
	runtime := t.BaselineRuntimeDigest
	expectation := p.BaselineExpectation
	if !receipt.Rollback {
		parts := strings.Split(receipt.After.Image, "@")
		if len(parts) != 2 {
			return ErrDeployment
		}
		runtime = parts[1]
		expectation = p.CandidateExpectation
	}
	if e.Validate(p, time.UnixMilli(h.ObservedAt)) != nil || e.Request.TargetSHA256 != t.Digest() || e.Request.SnapshotSHA256 != authority.Digest(raw) || e.Request.ImageDigest != runtime || e.Request.Expectation != expectation || h.StartedAt <= 0 || h.StartedAt > e.Request.IssuedAt || e.Request.ExpiresAt > h.StartedAt+browsercheck.MaxDuration.Milliseconds() || h.ObservedAt-h.StartedAt >= browsercheck.MaxDuration.Milliseconds() {
		return ErrDeployment
	}
	return nil
}
