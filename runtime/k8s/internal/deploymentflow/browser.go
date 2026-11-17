package deploymentflow

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"time"
)

func (t ConsoleTransport) VerifyBrowser(ctx context.Context, profile browsercheck.Profile, request browsercheck.Request, snapshot releasing.Snapshot, route releasing.Rollout, gate browsercheck.Gate) (releasing.Rollout, browsercheck.Evidence, error) {
	b, e := podtransport.NewBrowserResources(t.ConsoleDeployment, profile, request, route)
	if e != nil {
		return releasing.Rollout{}, browsercheck.Evidence{}, e
	}
	return b.Verify(ctx, snapshot, route, gate)
}

// HTTP checks, the fixed browser recipe, original-Pod deletion and durable
// recording share ONE smoke budget. No independent browser extension exists.
func (r Runtime) verifyHealth(ctx context.Context, j *store.DeploymentJournal, c store.DeploymentClaim, cfg releasing.Configuration, t Transport, snapshot releasing.Snapshot, phase string) (releasing.Health, error) {
	parent := ctx
	started := time.Now()
	ctx, cancel := context.WithDeadline(ctx, started.Add(time.Duration(cfg.Target.SmokeSeconds)*time.Second))
	defer cancel()
	health, e := t.VerifyHealth(ctx, snapshot)
	if e != nil {
		return releasing.Health{}, e
	}
	until, ok := ctx.Deadline()
	if !ok {
		return releasing.Health{}, ErrFlow
	}
	request, e := store.BrowserRequest(c, cfg.Browser, phase, snapshot, health.Rollout, until)
	if e != nil {
		return releasing.Health{}, e
	}
	gate := store.BrowserGate{CleanupParent: parent, Journal: j, Claim: c, Phase: phase, Snapshot: snapshot, Rollout: health.Rollout, Request: request}
	route, evidence, e := t.VerifyBrowser(ctx, cfg.Browser, request, snapshot, health.Rollout, gate)
	if e != nil || ctx.Err() != nil {
		return releasing.Health{}, ErrFlow
	}
	health.StartedAt = started.UnixMilli()
	health.Browser = evidence
	health.Rollout = route
	health.ObservedAt = time.Now().UnixMilli()
	if health.ObservedAt >= until.UnixMilli() {
		return releasing.Health{}, ErrFlow
	}
	return health, nil
}

// A fresh controller session can contain only a known resource. Expired app
// authority is never renewed and this path cannot finish a deployment.
func (r Runtime) cleanupBrowser(ctx context.Context, j *store.DeploymentJournal, original store.BrowserCleanup) (string, error) {
	started := time.Now()
	until := started.Add(browsercheck.CleanupBudget)
	ctx, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	gate := store.BrowserCleanupGate{Journal: j, Original: original, StartedAt: started, Until: until}
	if gate.CheckCleanup(ctx) != nil {
		return "browser-cleanup-authority", ErrFlow
	}
	cleaner, e := r.Cleaner()
	if e != nil || cleaner == nil {
		return "browser-cleanup-transport", ErrFlow
	}
	if cleaner.CleanupBrowser(ctx, original.Profile, original.Request, original.Rollout, original.PodUID, gate) != nil {
		return "browser-cleanup-uncertain", ErrFlow
	}
	return "browser-cleanup-recorded", nil
}
