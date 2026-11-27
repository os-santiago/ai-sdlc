// Package mergerecovery resolves an uncertain merge effect by a read-only
// observation of the forge. It never writes to the forge, never resumes or
// retries the original merge, and never weakens the journal: an effect changes
// only to a receipt (the pull request was merged exactly as planned) or to a
// known no-send (the pull request is provably still unmerged at the planned
// head, which is not part of main). Every other observation keeps merges
// quarantined. The request stays failed and no merged record is created.
package mergerecovery

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergegate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergewriter"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/merging"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/metrics"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// Observer is a read-only forge view; mergewriter.Reader implements it.
type Observer interface {
	Observe(context.Context, mergewriter.Plan) (mergewriter.Observation, error)
}

// Outcomes are the closed set of named results of one reconciliation step.
const (
	// Completed: the pull request was merged exactly as planned; the receipt
	// the transport would have returned is recorded.
	Completed = "completed"
	// NotSent: the pull request is still open and unmerged at the planned head
	// and that head is not part of main; a known no-send is recorded.
	NotSent = "not-sent"
	// Mismatch: the forge shows neither the planned merge nor the untouched
	// pull request. Quarantined.
	Mismatch = "mismatch"
	// Unavailable: the forge or the journal could not be read or written,
	// including a missing (404) or forbidden (403) read. Quarantined.
	Unavailable = "unavailable"
	// Unplanned: the journaled command does not re-derive the journaled
	// request digest. Quarantined.
	Unplanned = "unplanned"
	// Resolved: the effect was resolved elsewhere (another release-manager
	// replica, or an operator) before this step could record its outcome.
	Resolved = "resolved"
)

// Step reports one reconciliation attempt. It carries no body, token or URL.
type Step struct{ Outcome string }

var ErrRecovery = errors.New("merge reconciliation unavailable")

const (
	firstRetry = 30 * time.Second
	maxRetry   = 15 * time.Minute
	stepBudget = 30 * time.Second
)

// Reconciler holds only in-memory pacing: a quarantined effect is observed
// again after an exponential backoff (30 s doubling to 15 min), so a degraded
// forge sees a bounded read rate. The merge loop polls once a minute, so the
// first retries happen at the next poll. Pacing grants nothing; a restart only
// observes sooner.
type Reconciler struct {
	DB  *store.DB
	Now func() time.Time
	Log func(string, ...any)

	mu       sync.Mutex
	failures map[string]int
	next     map[string]time.Time
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Step reconciles at most one settled uncertain merge effect that is not
// pacing. It reports false when nothing was attempted.
func (r *Reconciler) Step(ctx context.Context, observer Observer) (Step, bool, error) {
	if r == nil || r.DB == nil || observer == nil {
		return Step{}, false, ErrRecovery
	}
	candidates, e := r.DB.MergeReconciliations(ctx, 16)
	if e != nil {
		return Step{}, false, ErrRecovery
	}
	r.mu.Lock()
	if r.failures == nil {
		r.failures, r.next = map[string]int{}, map[string]time.Time{}
	}
	if len(candidates) < 16 {
		// Forget pacing for effects resolved elsewhere.
		listed := map[string]bool{}
		for _, c := range candidates {
			listed[c.EffectToken] = true
		}
		for token := range r.next {
			if !listed[token] {
				delete(r.next, token)
				delete(r.failures, token)
			}
		}
	}
	now := r.now()
	var selected *store.MergeReconciliation
	for i := range candidates {
		if !now.Before(r.next[candidates[i].EffectToken]) {
			selected = &candidates[i]
			break
		}
	}
	r.mu.Unlock()
	if selected == nil {
		return Step{}, false, nil
	}
	step := Step{r.reconcile(ctx, observer, *selected)}
	r.pace(selected.EffectToken, step.Outcome)
	logf := r.Log
	if logf == nil {
		logf = log.Printf
	}
	// Bounded line: a closed outcome, nothing from the forge.
	logf("merge reconciliation: effect=merge outcome=%s", step.Outcome)
	metrics.MergeReconciliation(step.Outcome)
	return step, true, nil
}

// pace records the outcome of one step for an effect: an outcome that leaves
// nothing to observe (recorded here, or resolved elsewhere) forgets its
// pacing; any other outcome backs off exponentially.
func (r *Reconciler) pace(token, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failures == nil {
		r.failures, r.next = map[string]int{}, map[string]time.Time{}
	}
	if outcome == Completed || outcome == NotSent || outcome == Resolved {
		delete(r.failures, token)
		delete(r.next, token)
		return
	}
	r.failures[token]++
	wait := maxRetry
	if n := r.failures[token]; n < 6 {
		wait = min(firstRetry<<(n-1), maxRetry)
	}
	r.next[token] = r.now().Add(wait)
}

// Plan re-derives the exact merge request the journal recorded from the
// journaled signed command: its digest must equal the request digest
// journaled before the PUT.
func Plan(c store.MergeReconciliation) (mergewriter.Plan, bool) {
	var cmd merging.Command
	if authority.Digest(c.Payload) != c.PayloadDigest || authority.DecodeStrict(c.Payload, &cmd) != nil || cmd.Version != merging.Version || cmd.ID() != c.RequestID {
		return mergewriter.Plan{}, false
	}
	x := mergegate.Expected{Repository: cmd.Repository, PullRequest: cmd.PullRequest, HeadSHA: cmd.HeadSHA, BaseSHA: cmd.BaseSHA, TreeSHA: cmd.TreeSHA, Branch: cmd.Branch}
	p, e := mergewriter.PlanMerge(x, cmd.IssueNumber)
	if e != nil || p.RequestDigest != c.RequestDigest {
		return mergewriter.Plan{}, false
	}
	return p, true
}

func (r *Reconciler) reconcile(ctx context.Context, observer Observer, c store.MergeReconciliation) string {
	p, ok := Plan(c)
	if !ok {
		return Unplanned
	}
	ctx, cancel := context.WithTimeout(ctx, stepBudget)
	defer cancel()
	seen, e := observer.Observe(ctx, p)
	if e != nil {
		return Unavailable
	}
	switch seen.State {
	case mergewriter.ObservedMerged:
		// The receipt the transport returns after a verified merge, marked as
		// observed so that no live boundary can accept it.
		receipt := store.MergeReceipt{Version: "ai-sdlc.merge-receipt/v1", CommitSHA: seen.CommitSHA, HeadSHA: p.Expected.HeadSHA, BaseSHA: p.Expected.BaseSHA, TreeSHA: p.Expected.TreeSHA, PullRequest: p.Expected.PullRequest, Observed: true}
		return resolve(r.DB.ResolveMergeEffect(ctx, c, &receipt), Completed)
	case mergewriter.ObservedUnmerged:
		return resolve(r.DB.ResolveMergeEffect(ctx, c, nil), NotSent)
	case mergewriter.ObservedDifferent:
		return Mismatch
	}
	return Unavailable
}

func resolve(e error, outcome string) string {
	switch {
	case e == nil:
		return outcome
	case errors.Is(e, store.ErrMergeEffectResolved):
		return Resolved
	}
	return Unavailable
}
