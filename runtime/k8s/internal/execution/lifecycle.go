package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Placement is reviewed installation configuration, never issue/model content.
type Placement struct{ Namespace, WorkerImage, MailboxImage string }
type LifecycleLoader func(context.Context) (PreparationInputs, Placement, error)

// Lifecycle is an in-process, nonrenewable owner. There is deliberately no
// constructor from a durable row and no restart/adoption/retry API.
type Lifecycle struct {
	mu        sync.Mutex
	db        *store.DB
	load      LifecycleLoader
	prepared  Prepared
	placement Placement
	durable   store.Lifecycle
	stopped   bool
}

// EffectInput contains only immutable copies and previously acknowledged UIDs.
// A future trusted controller supplies fixed Kubernetes helpers, not callbacks
// obtained from repository code, issue fields or generated instructions.
type EffectInput struct {
	Kind, ConfigUID, PodUID           string
	Workload                          executor.Workload
	Assignment, Source, Profile, ADev []byte
}
type LifecycleOperation func(context.Context, EffectInput) (store.LifecycleResult, error)

func lifecycleInputs(load LifecycleLoader, placement Placement) PreparationLoader {
	return func(ctx context.Context) (PreparationInputs, error) {
		in, current, err := load(ctx)
		if err != nil || current != placement {
			return PreparationInputs{}, ErrAuthority
		}
		return in, nil
	}
}
func lifecycleBinding(p Prepared, placement Placement) (store.LifecycleBinding, executor.Workload, error) {
	w, err := executor.Compile(placement.Namespace, placement.WorkerImage, placement.MailboxImage, p.assignment, p.profile, p.adev, time.Now())
	if err != nil {
		return store.LifecycleBinding{}, executor.Workload{}, ErrAuthority
	}
	a, err := executor.ParseAssignment(p.assignment, workspacebundle.Digest(p.assignment), time.Now())
	if err != nil {
		return store.LifecycleBinding{}, executor.Workload{}, ErrAuthority
	}
	profile, err := executor.ParseProfile(p.profile, a.ProfileSHA256)
	if err != nil {
		return store.LifecycleBinding{}, executor.Workload{}, ErrAuthority
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return store.LifecycleBinding{}, executor.Workload{}, ErrAuthority
	}
	return store.LifecycleBinding{BindingDigest: workspacebundle.Digest(raw), SourceDigest: workspacebundle.Digest(p.source), Namespace: placement.Namespace,
		PodName: w.Pod["metadata"].(map[string]any)["name"].(string), ConfigName: w.ConfigMap["metadata"].(map[string]any)["name"].(string), MaxSeconds: profile.MaxSeconds, SourceBytes: len(p.source)}, w, nil
}
func samePrepared(p Prepared, c Command, in PreparationInputs) bool {
	a, err := executor.ParseAssignment(p.assignment, workspacebundle.Digest(p.assignment), time.Now())
	return err == nil && a.WorkItemID == c.WorkItemID && a.Attempt == c.Attempt && a.Repository == c.Repository && a.SourceSHA == c.SourceSHA && a.ProfileSHA256 == c.ProfileSHA256 && a.AuthorityRevision == c.AuthorityRevision && a.ExpiresAt == c.ExpiresAt && bytes.Equal(in.Profile, p.profile) && bytes.Equal(in.ADev, p.adev)
}
func stopLifecycleClaim(db *store.DB, claim store.ExecutionClaim) {
	if db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = db.StopExecutionClaim(ctx, claim, "uncertain")
}

// OpenLifecycle revalidates preparation, commits a unique bounded lifetime and
// revalidates again. Any ambiguous result stops only this preparation's token.
func OpenLifecycle(ctx context.Context, db *store.DB, load LifecycleLoader, p Prepared) (result *Lifecycle, err error) {
	if db == nil || load == nil || p.claim.Token == "" {
		return nil, ErrAuthority
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	defer func() {
		if err != nil {
			stopLifecycleClaim(db, p.claim)
		}
	}()
	_, placement, err := load(ctx)
	if err != nil {
		return nil, ErrAuthority
	}
	c, _, in, err := preparationProof(ctx, db, lifecycleInputs(load, placement), p.claim)
	if err != nil || !samePrepared(p, c, in) {
		return nil, ErrAuthority
	}
	binding, _, err := lifecycleBinding(p, placement)
	if err != nil {
		return nil, ErrAuthority
	}
	durable, err := db.ReserveLifecycle(ctx, p.claim, binding)
	if err != nil {
		return nil, ErrAuthority
	}
	p.claim = durable.Claim
	l := &Lifecycle{db: db, load: load, prepared: p, placement: placement, durable: durable}
	if _, err = l.proof(ctx); err != nil {
		return nil, ErrAuthority
	}
	return l, nil
}
func (l *Lifecycle) proof(ctx context.Context) (executor.Workload, error) {
	c, _, in, err := preparationProof(ctx, l.db, lifecycleInputs(l.load, l.placement), l.prepared.claim)
	if err != nil || !samePrepared(l.prepared, c, in) {
		return executor.Workload{}, ErrAuthority
	}
	binding, w, err := lifecycleBinding(l.prepared, l.placement)
	if err != nil || binding != l.durable.Binding {
		return executor.Workload{}, ErrAuthority
	}
	return w, nil
}

// RunEffect persists intent before calling the trusted effect exactly once in
// this process. Database ordering also excludes other owners/processes. An
// uncertain callback or COMMIT stops the parent; no effect is ever replayed.
// Context cancellation does not guarantee remote rollback or termination.
func (l *Lifecycle) RunEffect(ctx context.Context, kind string, operation LifecycleOperation) (err error) {
	if l == nil || operation == nil {
		return ErrAuthority
	}
	if !l.mu.TryLock() {
		return ErrAuthority
	}
	defer l.mu.Unlock()
	if l.stopped {
		return ErrAuthority
	}
	ctx, cancel := context.WithDeadline(ctx, l.durable.Claim.Until)
	defer cancel()
	defer func() {
		if recover() != nil {
			err = ErrAuthority
		}
		if err != nil {
			l.stopped = true
			stopLifecycleClaim(l.db, l.durable.Claim)
		}
	}()
	if _, err = l.proof(ctx); err != nil {
		return ErrAuthority
	}
	effect, err := l.db.BeginLifecycleEffect(ctx, l.durable, kind)
	if err != nil {
		return ErrAuthority
	}
	w, err := l.proof(ctx)
	if err != nil {
		return ErrAuthority
	}
	input := EffectInput{Kind: kind, ConfigUID: effect.ConfigUID, PodUID: effect.PodUID, Workload: w,
		Assignment: l.prepared.Assignment(), Profile: l.prepared.Profile(), ADev: l.prepared.ADev()}
	if kind == "source" {
		input.Source = l.prepared.Source()
	}
	if ctx.Err() != nil {
		return ErrAuthority
	}
	observed, err := operation(ctx, input)
	if observed.ExitCode != nil {
		code := *observed.ExitCode
		observed.ExitCode = &code
	}
	if err != nil || ctx.Err() != nil {
		return ErrAuthority
	}
	if _, err = l.proof(ctx); err != nil {
		return ErrAuthority
	}
	if err = l.db.CompleteLifecycleEffect(ctx, effect, observed); err != nil {
		return ErrAuthority
	}
	if _, err = l.proof(ctx); err != nil {
		return ErrAuthority
	}
	return nil
}
