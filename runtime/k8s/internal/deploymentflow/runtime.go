package deploymentflow

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"time"
)

// Transport is a platform-owned adapter, never a model or issue input.
type Transport interface {
	Observe(context.Context) (releasing.Snapshot, error)
	Apply(context.Context, releasing.Plan, releasing.Snapshot, podtransport.ConsoleEffectGate) (releasing.Receipt, error)
	Rollback(context.Context, releasing.Receipt, releasing.Snapshot, podtransport.ConsoleEffectGate) (releasing.Receipt, error)
	WaitReady(context.Context, releasing.Snapshot) (releasing.Rollout, error)
	VerifyHealth(context.Context, releasing.Snapshot) (releasing.Health, error)
}
type ConsoleTransport struct {
	*podtransport.ConsoleDeployment
}

func (t ConsoleTransport) Apply(ctx context.Context, p releasing.Plan, before releasing.Snapshot, g podtransport.ConsoleEffectGate) (releasing.Receipt, error) {
	return (&podtransport.ConsoleEffects{Deployment: t.ConsoleDeployment, Gate: g}).Apply(ctx, p, before)
}
func (t ConsoleTransport) Rollback(ctx context.Context, r releasing.Receipt, before releasing.Snapshot, g podtransport.ConsoleEffectGate) (releasing.Receipt, error) {
	return (&podtransport.ConsoleEffects{Deployment: t.ConsoleDeployment, Gate: g}).Rollback(ctx, r, before)
}

type Runtime struct {
	DB   *store.DB
	Load func() (releasing.Configuration, error)
	// Target loads the independent installed destination even if current advance
	// policies/trust are unavailable. It cannot choose or renew an original target.
	Target    func() (releasing.Target, error)
	Transport func(releasing.Target) (Transport, error)
}

func (r Runtime) journal() *store.DeploymentJournal {
	return store.NewDeploymentJournalWithTarget(r.DB, r.Load, r.Target)
}
func (r Runtime) PollOnce(ctx context.Context) (string, error) {
	if r.DB == nil || r.Load == nil || r.Target == nil || r.Transport == nil {
		return "", ErrFlow
	}
	state := "deployment-idle"
	acquired, e := r.DB.WithDeploymentController(ctx, func(active context.Context) error { var err error; state, err = r.pollExclusive(active); return err })
	if !acquired && e == nil {
		return "deployment-other-replica", nil
	}
	return state, e
}
func (r Runtime) pollExclusive(ctx context.Context) (string, error) {
	j := r.journal()
	if e := j.ExpireDeployments(ctx); e != nil {
		return "deployment-expiration", e
	}
	recovery, e := j.NextDeploymentRecovery(ctx)
	if e != nil {
		return "deployment-recovery-selection", e
	}
	if recovery.ID != "" {
		return r.recover(ctx, j, recovery)
	}
	id, e := j.NextDeployment(ctx)
	if e != nil {
		return "deployment-authority", e
	}
	if id == "" {
		return "deployment-idle", nil
	}
	claim, e := j.ClaimDeployment(ctx, id)
	if e != nil {
		return "deployment-claim", e
	}
	stop := func(reason string) {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = j.QuarantineDeployment(cleanup, claim, reason)
	}
	cfg, input, e := j.ReadDeploymentInput(ctx, claim)
	if e != nil {
		stop("revoked")
		return "deployment-authority", ErrFlow
	}
	// Every network operation, including containment and health, inherits the
	// original command deadline. The longer target approval never extends it.
	attempt, cancel := context.WithDeadline(ctx, claim.Until)
	defer cancel()
	transport, e := r.Transport(cfg.Target)
	if e != nil || transport == nil {
		stop("rejected")
		return "deployment-configuration", ErrFlow
	}
	before, e := transport.Observe(attempt)
	if e != nil || before.Image != cfg.Target.BaselineImage {
		stop("rejected")
		return "deployment-baseline", ErrFlow
	}
	if _, e = transport.VerifyHealth(attempt, before); e != nil {
		stop("unhealthy")
		return "deployment-baseline-health", ErrFlow
	}
	before, e = transport.Observe(attempt)
	if e != nil || before.Image != cfg.Target.BaselineImage {
		stop("rejected")
		return "deployment-baseline-changed", ErrFlow
	}
	if e = j.BeginDeploymentEffect(attempt, claim, before, false); e != nil {
		stop("revoked")
		return "deployment-intent", ErrFlow
	}
	gate := store.DeploymentGate{Journal: j, Claim: claim}
	applied, e := transport.Apply(attempt, input.Promotion.Plan, before, gate)
	if e != nil {
		stop("uncertain")
		return "deployment-uncertain", ErrFlow
	}
	// Resolve only this exact acknowledged response if recording loses COMMIT ACK.
	if e = r.recordReceipt(attempt, j, claim, applied); e != nil {
		stop("uncertain")
		return "deployment-acknowledgement", ErrFlow
	}
	if _, e = transport.WaitReady(attempt, applied.After); e == nil {
		var h releasing.Health
		h, e = transport.VerifyHealth(attempt, applied.After)
		if e == nil {
			e = j.FinishDeployment(attempt, claim, h, false)
			if e == nil {
				return "deployment-recorded", nil
			}
			if recorded, readErr := j.ReadDeploymentRecord(attempt, claim); readErr == nil && recorded.Outcome == "deployed" && recorded.Receipt == applied && recorded.Health.Digest() == h.Digest() {
				return "deployment-recorded", nil
			}
		}
	}
	return r.contain(attempt, j, claim, transport, applied, stop)
}
func (r Runtime) recordReceipt(ctx context.Context, j *store.DeploymentJournal, c store.DeploymentClaim, receipt releasing.Receipt) error {
	if e := j.RecordDeploymentReceipt(ctx, c, receipt); e != nil {
		stored, readErr := j.ReadDeploymentReceipt(ctx, c, receipt.Rollback)
		if readErr != nil || stored != receipt {
			return ErrFlow
		}
	}
	return nil
}
func (r Runtime) contain(ctx context.Context, j *store.DeploymentJournal, c store.DeploymentClaim, t Transport, original releasing.Receipt, stop func(string)) (string, error) {
	// Context must still be alive. Expiry/cancellation is quarantine, not an
	// implicit emergency extension or a new background deployment authority.
	if ctx.Err() != nil || !c.Until.After(time.Now()) {
		stop("expired")
		return "deployment-containment-expired", ErrFlow
	}
	current, e := t.Observe(ctx)
	if e != nil {
		stop("uncertain")
		return "deployment-containment-observation", ErrFlow
	}
	if e = j.BeginDeploymentEffect(ctx, c, current, true); e != nil {
		stop("revoked")
		return "deployment-containment-authority", ErrFlow
	}
	rolled, e := t.Rollback(ctx, original, current, store.DeploymentGate{Journal: j, Claim: c})
	if e != nil {
		stop("uncertain")
		return "deployment-containment-uncertain", ErrFlow
	}
	if e = r.recordReceipt(ctx, j, c, rolled); e != nil {
		stop("uncertain")
		return "deployment-containment-acknowledgement", ErrFlow
	}
	return r.finishContainment(ctx, j, c, t, rolled, stop)
}
func (r Runtime) finishContainment(ctx context.Context, j *store.DeploymentJournal, c store.DeploymentClaim, t Transport, rolled releasing.Receipt, stop func(string)) (string, error) {
	if _, e := t.WaitReady(ctx, rolled.After); e != nil {
		stop("unhealthy")
		return "deployment-containment-readiness", ErrFlow
	}
	health, e := t.VerifyHealth(ctx, rolled.After)
	if e != nil {
		stop("unhealthy")
		return "deployment-containment-health", ErrFlow
	}
	if e = j.FinishDeployment(ctx, c, health, true); e != nil {
		recorded, readErr := j.ReadDeploymentRecord(ctx, c)
		if readErr != nil || recorded.Outcome != "rolled-back" || recorded.Receipt != rolled || recorded.Health.Digest() != health.Digest() {
			stop("uncertain")
			return "deployment-containment-completion", ErrFlow
		}
	}
	return "deployment-rolled-back", nil
}
