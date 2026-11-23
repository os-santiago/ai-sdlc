package execution

import (
	"context"
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// ModelPolicy is reviewed installation configuration. Limits count calls/bytes,
// not provider tokens or money. The loader must reread current configuration.
type ModelPolicy struct {
	Limits        store.ModelLimits
	ResponseLimit int
}
type ModelPolicyLoader func(context.Context) (ModelPolicy, error)
type lifecycleModel struct {
	policy ModelPolicy
	load   ModelPolicyLoader
	scope  store.ModelScope
	digest string
}

func (p ModelPolicy) valid() bool {
	return p.Limits.Calls > 0 && p.Limits.Calls <= 32 && p.Limits.InputBytes > 0 && p.Limits.InputBytes <= 4194304 && p.Limits.OutputBytes > 0 && p.Limits.OutputBytes <= 8388608 && p.ResponseLimit > 0 && p.ResponseLimit <= modelmailbox.MaxResponseBytes && int64(p.ResponseLimit) <= p.Limits.OutputBytes
}
func sameModelScope(a, b store.ModelScope) bool {
	if !a.ExpiresAt.Equal(b.ExpiresAt) {
		return false
	}
	a.ExpiresAt = time.Time{}
	b.ExpiresAt = time.Time{}
	return a == b
}
func (l *Lifecycle) modelProof(ctx context.Context) (string, error) {
	if l.model == nil || l.model.load == nil {
		return "", ErrAuthority
	}
	current, err := l.model.load(ctx)
	if err != nil || current != l.model.policy || !current.valid() {
		return "", ErrAuthority
	}
	scope, phase, err := l.db.ReadLifecycleModel(ctx, l.durable, l.model.digest)
	if err != nil || !sameModelScope(scope, l.model.scope) {
		return "", ErrAuthority
	}
	current, err = l.model.load(ctx)
	if err != nil || current != l.model.policy || ctx.Err() != nil {
		return "", ErrAuthority
	}
	return phase, nil
}

// ActivateModel is a one-shot ownership transition. No result is adopted after
// a failed or uncertain commit; only this exact lifecycle claim is stopped.
func (l *Lifecycle) ActivateModel(ctx context.Context, load ModelPolicyLoader) (scope store.ModelScope, err error) {
	if l == nil || load == nil || !l.mu.TryLock() {
		return store.ModelScope{}, ErrAuthority
	}
	defer l.mu.Unlock()
	if l.stopped || l.model != nil {
		return store.ModelScope{}, ErrAuthority
	}
	ctx, cancel := context.WithDeadline(ctx, l.durable.Claim.Until)
	defer cancel()
	ctx, stopBound := context.WithTimeout(ctx, 20*time.Second)
	defer stopBound()
	defer func() {
		if recover() != nil {
			err = ErrAuthority
		}
		if err != nil {
			scope = store.ModelScope{}
			l.stopped = true
			stopLifecycleClaim(l.db, l.durable.Claim)
		}
	}()
	if _, err = l.proof(ctx); err != nil {
		return store.ModelScope{}, ErrAuthority
	}
	policy, err := load(ctx)
	if err != nil || !policy.valid() {
		return store.ModelScope{}, ErrAuthority
	}
	a, err := executor.ParseAssignment(l.prepared.assignment, workspacebundle.Digest(l.prepared.assignment), time.Now())
	if err != nil {
		return store.ModelScope{}, ErrAuthority
	}
	p, err := executor.ParseProfile(l.prepared.profile, a.ProfileSHA256)
	if err != nil {
		return store.ModelScope{}, ErrAuthority
	}
	// The store derives the original acknowledged Pod UID; callers cannot select
	// a different Kubernetes execution for this allocation.
	pod, err := l.db.LifecyclePodForModel(ctx, l.durable)
	if err != nil {
		return store.ModelScope{}, ErrAuthority
	}
	current, err := load(ctx)
	if err != nil || current != policy {
		return store.ModelScope{}, ErrAuthority
	}
	if _, err = l.proof(ctx); err != nil {
		return store.ModelScope{}, ErrAuthority
	}
	raw, _ := json.Marshal(policy)
	digest := workspacebundle.Digest(raw)
	b := store.ModelBudget{WorkItem: a.WorkItemID, Repository: a.Repository, SourceSHA: a.SourceSHA, PolicyDigest: a.ProfileSHA256, Model: p.Model, Limits: policy.Limits, ExpiresAt: l.durable.Claim.Until}
	allocation := store.ModelAllocation{ID: store.NewID(), WorkItem: a.WorkItemID, PodUID: pod, Attempt: a.Attempt, Limits: policy.Limits, ResponseLimit: policy.ResponseLimit, ExpiresAt: l.durable.Claim.Until}
	if a.Repair != nil {
		b, allocation, err = l.db.RepairModelReservation(ctx, l.durable.Claim.ID, digest)
		if err != nil || b.WorkItem != a.WorkItemID || b.Repository != a.Repository || b.SourceSHA != a.SourceSHA || b.PolicyDigest != a.ProfileSHA256 || b.Model != p.Model ||
			b.Limits.Calls != a.Repair.RemainingCalls || b.Limits.Calls > policy.Limits.Calls || b.Limits.InputBytes > policy.Limits.InputBytes || b.Limits.OutputBytes > policy.Limits.OutputBytes || allocation.ResponseLimit != policy.ResponseLimit {
			return store.ModelScope{}, ErrAuthority
		}
		if l.durable.Claim.Until.Before(b.ExpiresAt) {
			b.ExpiresAt = l.durable.Claim.Until
		}
		allocation.PodUID, allocation.ExpiresAt = pod, b.ExpiresAt
		scope, err = l.db.ActivateRepairLifecycleModel(ctx, l.durable, b, allocation, digest)
	} else {
		scope, err = l.db.ActivateLifecycleModel(ctx, l.durable, b, allocation, digest)
	}
	if err != nil {
		return store.ModelScope{}, ErrAuthority
	}
	l.model = &lifecycleModel{policy: policy, load: load, scope: scope, digest: digest}
	if _, err = l.proof(ctx); err != nil {
		return store.ModelScope{}, ErrAuthority
	}
	return scope, nil
}

// Authorize implements the broker's current-authority capability. An allocation
// alone never authorizes a call, and no call is allowed before start acknowledgement
// or after candidate collection begins.
func (l *Lifecycle) Authorize(ctx context.Context, scope store.ModelScope) (until time.Time, err error) {
	if l == nil || !l.mu.TryLock() {
		return time.Time{}, authorityStop("busy")
	}
	defer l.mu.Unlock()
	if l.stopped {
		return time.Time{}, authorityStop("stopped")
	}
	if l.model == nil || !sameModelScope(scope, l.model.scope) {
		return time.Time{}, authorityStop("scope")
	}
	ctx, cancel := context.WithDeadline(ctx, l.durable.Claim.Until)
	defer cancel()
	ctx, stopBound := context.WithTimeout(ctx, 5*time.Second)
	defer stopBound()
	defer func() {
		if recover() != nil {
			until = time.Time{}
			err = authorityStop("panic")
		}
	}()
	_, phase, err := l.proofPhase(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return time.Time{}, authorityStop("deadline")
		}
		return time.Time{}, err
	}
	if phase != "start-ready" {
		return time.Time{}, authorityStop("phase")
	}
	if ctx.Err() != nil {
		return time.Time{}, authorityStop("deadline")
	}
	return l.durable.Claim.Until, nil
}

// authorityStop names the bounded check that refused current execution
// authority. It unwraps to ErrAuthority, so every caller refuses as before;
// the step only refines the stop diagnostic (AuthorityStep).
type authorityStop string

func (e authorityStop) Error() string         { return "execution authority refused at " + string(e) }
func (e authorityStop) Unwrap() error         { return ErrAuthority }
func (e authorityStop) AuthorityStep() string { return string(e) }
