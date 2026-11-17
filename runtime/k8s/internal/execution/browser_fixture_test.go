package execution

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"strings"
	"testing"
	"time"
)

// Synthetic adapter facts exercise PostgreSQL authority only. Real Firefox,
// API identity, network policy and cleanup are separate Kubernetes fixtures.
func syntheticBrowser(ctx context.Context, p browsercheck.Profile, r browsercheck.Request, g browsercheck.Gate) (browsercheck.Evidence, error) {
	var evidence browsercheck.Evidence
	if e := g.BeforeCreate(ctx, strings.Repeat("a", 64), strings.Repeat("e", 64)); e != nil {
		return evidence, e
	}
	pod := store.NewID()
	if e := g.AcknowledgePod(ctx, r.RequestID, pod, strings.Repeat("e", 64)); e != nil {
		return evidence, e
	}
	now := time.Now().UnixMilli()
	result := browsercheck.Result{Version: browsercheck.ResultVersion, Status: "passed", RequestID: r.RequestID, RequestSHA256: r.Digest(), TargetSHA256: r.TargetSHA256, SnapshotSHA256: r.SnapshotSHA256, ImageDigest: r.ImageDigest, Expectation: r.Expectation, StartedAt: now, FinishedAt: now, Observations: []browsercheck.Observation{{Viewport: "desktop", ResponseSHA256: strings.Repeat("b", 64)}, {Viewport: "mobile", ResponseSHA256: strings.Repeat("c", 64)}}, NormalTLS: true, NetworkScopePreserved: true}
	evidence = browsercheck.Evidence{ProfileSHA256: p.Digest(), Request: r, Result: result, PodUID: pod, ContainerID: "containerd://" + strings.Repeat("d", 64), RuntimeDigest: p.RuntimeDigest, DeletedAt: now}
	return evidence, g.RecordEvidence(ctx, evidence)
}
func (d *deploymentTransportFixture) VerifyBrowser(ctx context.Context, p browsercheck.Profile, r browsercheck.Request, s releasing.Snapshot, route releasing.Rollout, g browsercheck.Gate) (releasing.Rollout, browsercheck.Evidence, error) {
	d.deadline(ctx)
	evidence, e := syntheticBrowser(ctx, p, r, g)
	return route, evidence, e
}
func (f deploymentFixture) proveBrowser(t *testing.T, c store.DeploymentClaim, phase string, s releasing.Snapshot, h releasing.Health) releasing.Health {
	t.Helper()
	h.StartedAt = h.Rollout.ObservedAt
	r, e := store.BrowserRequest(c, f.p.cfg.Browser, phase, s, h.Rollout, time.UnixMilli(h.StartedAt).Add(30*time.Second))
	if e != nil {
		t.Fatal("synthetic browser request", e)
	}
	g := store.BrowserGate{Journal: f.j, Claim: c, Phase: phase, Snapshot: s, Rollout: h.Rollout, Request: r}
	h.Browser, e = syntheticBrowser(context.Background(), f.p.cfg.Browser, r, g)
	if e != nil {
		t.Fatal("synthetic browser evidence", phase, e)
	}
	h.ObservedAt = time.Now().UnixMilli()
	return h
}
func (f deploymentFixture) ensureBaseline(t *testing.T, c store.DeploymentClaim, s releasing.Snapshot) {
	t.Helper()
	if f.p.b.f.Count(t, `SELECT count(*) FROM deployment_browser_evidence e JOIN deployment_browser_intents i ON i.id=e.id WHERE i.phase='baseline'`) == 0 {
		f.proveBrowser(t, c, "baseline", s, f.health(releasing.Receipt{After: s, Rollback: true}))
	}
}
func (f deploymentFixture) attestedHealth(t *testing.T, c store.DeploymentClaim, r releasing.Receipt) releasing.Health {
	t.Helper()
	phase := "advance"
	if r.Rollback {
		phase = "rollback"
	}
	return f.proveBrowser(t, c, phase, r.After, f.health(r))
}

func (d *deploymentTransportFixture) CleanupBrowser(ctx context.Context, p browsercheck.Profile, r browsercheck.Request, route releasing.Rollout, uid string, g browsercheck.CleanupGate) error {
	if g.CheckCleanup(ctx) != nil {
		return browsercheck.ErrBrowser
	}
	return g.RecordCleanup(ctx)
}
