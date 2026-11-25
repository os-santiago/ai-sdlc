// Package publicationrecovery resolves an uncertain publication effect by a
// read-only observation of the forge. It never writes to the forge, never
// resumes or retries the original publication, and never weakens the journal:
// an effect changes only to a receipt (it exists exactly as planned) or to a
// known no-send (the forge proves it absent). Every other observation keeps
// the publication quarantined.
package publicationrecovery

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publication"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/sourcefetch"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Observer is a read-only forge view; publication.Reader implements it.
type Observer interface {
	Observe(context.Context, publication.Expectation) (publication.Observation, error)
}

// SourceLoader reads the publication's immutable source snapshot.
type SourceLoader func(context.Context, string, string) (workspacebundle.Bundle, error)

// Outcomes are the closed set of named results of one reconciliation step.
const (
	// Completed: the effect exists exactly as planned; its receipt is recorded.
	Completed = "completed"
	// NotSent: the forge proves the effect absent; a known no-send is recorded.
	NotSent = "not-sent"
	// Mismatch: something exists that is not the planned effect. Quarantined.
	Mismatch = "mismatch"
	// Unavailable: the forge, source or journal could not be read or written. Quarantined.
	Unavailable = "unavailable"
	// Unplanned: the journaled facts do not re-derive the journaled request. Quarantined.
	Unplanned = "unplanned"
)

// Step reports one reconciliation attempt. It carries no body, token or URL.
type Step struct{ Kind, Outcome string }

var ErrRecovery = errors.New("publication reconciliation unavailable")

const (
	firstRetry = 30 * time.Second
	maxRetry   = 15 * time.Minute
	stepBudget = 60 * time.Second
)

// Reconciler holds only in-memory pacing: a quarantined effect is observed
// again after an exponential backoff (30 s doubling to 15 min), so a degraded
// forge sees a bounded read rate. Pacing grants nothing; a restart only
// observes sooner.
type Reconciler struct {
	DB *store.DB
	// Fetch reads the pinned source to rebuild a tree plan (default: the
	// public source fetch the controller uses).
	Fetch SourceLoader
	// Plan, when set, replaces the default tree-plan rebuild (candidate delta
	// applied to the fetched source). Whatever it returns must still match the
	// claim's journaled plan digest and the effect's request digest.
	Plan func(context.Context, publication.Command) (publication.TreePlan, error)
	Now  func() time.Time
	Log  func(string, ...any)

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

func fetchSource(ctx context.Context, repo, sha string) (workspacebundle.Bundle, error) {
	client, e := sourcefetch.New(repo, "")
	if e != nil {
		return workspacebundle.Bundle{}, e
	}
	return client.Fetch(ctx, sha)
}

var errUnavailable = errors.New("publication plan evidence unavailable")

// prepare rebuilds the tree plan exactly as the controller did: the immutable
// candidate delta applied to the pinned source. A read failure is
// unavailability; a plan the evidence cannot rebuild is ErrPublication.
func (r *Reconciler) prepare(ctx context.Context, cmd publication.Command) (publication.TreePlan, error) {
	candidate, e := r.DB.ReadPublicationCandidate(ctx, cmd.CandidateID)
	if e != nil {
		return publication.TreePlan{}, errUnavailable
	}
	fetch := r.Fetch
	if fetch == nil {
		fetch = fetchSource
	}
	read, stop := context.WithTimeout(ctx, 40*time.Second)
	source, e := fetch(read, cmd.Repository, cmd.SourceSHA)
	stop()
	if e != nil {
		return publication.TreePlan{}, errUnavailable
	}
	plan, e := publication.PrepareTree(cmd, source, candidate.Delta)
	if e != nil {
		return publication.TreePlan{}, publication.ErrPublication
	}
	return plan, nil
}

// Step reconciles at most one settled uncertain effect that is not pacing.
// It reports false when nothing was attempted.
func (r *Reconciler) Step(ctx context.Context, observer Observer) (Step, bool, error) {
	if r == nil || r.DB == nil || observer == nil {
		return Step{}, false, ErrRecovery
	}
	candidates, e := r.DB.PublicationReconciliations(ctx, 16)
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
	var selected *store.PublicationReconciliation
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
	step := Step{selected.Kind, r.reconcile(ctx, observer, *selected)}
	r.mu.Lock()
	if step.Outcome == Completed || step.Outcome == NotSent {
		delete(r.failures, selected.EffectToken)
		delete(r.next, selected.EffectToken)
	} else {
		r.failures[selected.EffectToken]++
		wait := maxRetry
		if n := r.failures[selected.EffectToken]; n < 6 {
			wait = min(firstRetry<<(n-1), maxRetry)
		}
		r.next[selected.EffectToken] = r.now().Add(wait)
	}
	r.mu.Unlock()
	logf := r.Log
	if logf == nil {
		logf = log.Printf
	}
	// Bounded line: a journal kind and a closed outcome, nothing from the forge.
	logf("publication reconciliation: effect=%s outcome=%s", step.Kind, step.Outcome)
	return step, true, nil
}

func (r *Reconciler) reconcile(ctx context.Context, observer Observer, c store.PublicationReconciliation) string {
	ctx, cancel := context.WithTimeout(ctx, stepBudget)
	defer cancel()
	x, outcome := r.expect(ctx, c)
	if outcome != "" {
		return outcome
	}
	seen, e := observer.Observe(ctx, x)
	if e != nil {
		return Unavailable
	}
	switch seen.State {
	case publication.ObservedExact:
		receipt := store.PublicationReceipt{Version: "ai-sdlc.publication-receipt/v1", Kind: c.Kind, SHA: x.SHA, Observed: true}
		if c.Kind == publication.EffectBranch || c.Kind == publication.EffectPullRequest {
			receipt.Branch = x.Branch
		}
		if c.Kind == publication.EffectPullRequest {
			if seen.PullRequest < 1 {
				return Mismatch
			}
			receipt.PullRequest = seen.PullRequest
		}
		if r.DB.ResolvePublicationEffect(ctx, c, &receipt) != nil {
			return Unavailable
		}
		return Completed
	case publication.ObservedAbsent:
		if r.DB.ResolvePublicationEffect(ctx, c, nil) != nil {
			return Unavailable
		}
		return NotSent
	case publication.ObservedDifferent:
		return Mismatch
	}
	return Unavailable
}

// expect re-derives the exact write the journal recorded: the signed command,
// the claim's plan digest and commit time, and acknowledged predecessors. The
// derived request digest must equal the one journaled before the POST.
func (r *Reconciler) expect(ctx context.Context, c store.PublicationReconciliation) (publication.Expectation, string) {
	var cmd publication.Command
	if authority.Digest(c.Payload) != c.PayloadDigest || authority.DecodeStrict(c.Payload, &cmd) != nil || cmd.ID() != c.RequestID || cmd.Repository != c.Repository {
		return publication.Expectation{}, Unplanned
	}
	receipt := func(i int) store.PublicationReceipt {
		if i < len(c.Completed) {
			return c.Completed[i].Receipt
		}
		return store.PublicationReceipt{}
	}
	var x publication.Expectation
	var e error
	switch c.Kind {
	case publication.EffectTree:
		plan := r.Plan
		if plan == nil {
			plan = r.prepare
		}
		p, err := plan(ctx, cmd)
		if errors.Is(err, publication.ErrPublication) {
			return x, Unplanned
		}
		if err != nil {
			return x, Unavailable
		}
		// The claim journaled this digest of the command and its whole plan.
		if publication.PlanDigest(cmd, p) != c.PlanDigest {
			return x, Unplanned
		}
		x, e = publication.ExpectTree(p, cmd.SourceSHA)
	case publication.EffectCommit:
		x, e = publication.ExpectCommit(cmd.Repository, cmd.IssueNumber, receipt(0).SHA, cmd.SourceSHA, c.CommitTime)
	case publication.EffectBranch:
		x, e = publication.ExpectBranch(cmd.Repository, publication.BranchName(cmd), receipt(1).SHA, cmd.SourceSHA)
	case publication.EffectPullRequest:
		branch := receipt(2)
		if branch.Branch != publication.BranchName(cmd) || branch.SHA != receipt(1).SHA {
			return x, Unplanned
		}
		x, e = publication.ExpectPullRequest(cmd.Repository, cmd.IssueNumber, branch.Branch, branch.SHA, c.PlanDigest, cmd.SourceSHA)
	default:
		return x, Unplanned
	}
	if e != nil || x.RequestDigest != c.RequestDigest {
		return publication.Expectation{}, Unplanned
	}
	return x, ""
}
