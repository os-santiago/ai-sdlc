// Package mergeflow joins historical publication facts to fresh issue, policy
// and forge checks. Only the explicitly enabled release-manager loop calls it;
// it has no external route.
package mergeflow

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergegate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergerecovery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergewriter"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/merging"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publication"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publicationflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
)

var ErrMerge = errors.New("merge controller authority unavailable or denied")

type Gates interface {
	Collect(context.Context, mergegate.Expected, mergegate.Policy) (mergegate.Observation, error)
}
type Transport interface {
	Merge(context.Context, mergegate.Expected, int, mergewriter.Fence) (mergewriter.Receipt, error)
}
type Runtime struct {
	DB         *store.DB
	Policy     verificationflow.PolicyLoader
	Review     publicationflow.ReviewLoader
	Identity   func() (authority.Identity, error)
	GatePolicy func(projectSHA256 string) (mergegate.Policy, error)
	Gates      Gates
	Writer     Transport
	// Reconciler and Observer, when both set, resolve an uncertain merge
	// effect that quarantines merges by a read-only forge observation.
	Reconciler *mergerecovery.Reconciler
	Observer   mergerecovery.Observer
	// Rotation, when set, orders selection so a publication whose gate stays
	// unready cannot hold later ones, and parks one whose pull request was
	// closed. It must outlive a single poll.
	Rotation *Rotation
	// Waiting, when set, receives the bounded blockers of a publication whose
	// gate is not ready, so the caller can log and count them. It never
	// carries forge text.
	Waiting func(Wait)
}

// Wait describes a published pull request the gate holds. Reasons is the
// sorted closed set of gate reasons (mergegate.Reasons); UnresolvedThreads is
// the gate's count of unresolved review threads.
type Wait struct {
	State             string
	PullRequest       int
	Reasons           []string
	UnresolvedThreads int
	// UnresolvedThreadsUnknown: the pull request has more review threads
	// than the gate can see, so UnresolvedThreads is 0 and the count unknown.
	UnresolvedThreadsUnknown bool
}

// waitStates maps each gate reason to the wait state that names it, in the
// order a wait state is chosen when several blockers hold one pull request:
// the first present reason names the state, and Wait.Reasons lists them all.
// native-merge-not-ready comes last because the forge reports it for any of
// the other blockers too.
var waitStates = []struct{ reason, state string }{
	{mergegate.ReasonNativeProtectionInsufficient, "merge-waiting-for-protection"},
	{mergegate.ReasonChecksNotPassed, "merge-waiting-for-checks"},
	{mergegate.ReasonChangesRequested, "merge-waiting-for-changes-requested"},
	{mergegate.ReasonThreadsUnresolved, "merge-waiting-for-review-threads"},
	{mergegate.ReasonNativeMergeNotReady, "merge-waiting-for-mergeability"},
}

// WaitStates is the closed set of states PollOnce returns for a gate that is
// not ready. "merge-waiting" names a gate that is not ready without a known
// reason.
func WaitStates() []string {
	out := []string{}
	for _, w := range waitStates {
		out = append(out, w.state)
	}
	return append(out, "merge-waiting")
}

// waitFor names the blockers of an observation that is not ready. Reasons
// outside the closed set are dropped, so nothing else reaches a log.
func waitFor(o mergegate.Observation) Wait {
	present := map[string]bool{}
	for _, reason := range o.Reasons {
		present[reason] = true
	}
	w := Wait{State: "merge-waiting", PullRequest: o.Expected.PullRequest, Reasons: []string{}}
	for _, known := range mergegate.Reasons {
		if present[known] {
			w.Reasons = append(w.Reasons, known)
		}
	}
	for _, candidate := range waitStates {
		if present[candidate.reason] {
			w.State = candidate.state
			break
		}
	}
	if present[mergegate.ReasonThreadsUnresolved] {
		switch {
		case o.UnresolvedThreadsUnknown:
			w.UnresolvedThreadsUnknown = true
		case o.UnresolvedThreads > 0 && o.UnresolvedThreads <= 100:
			w.UnresolvedThreads = o.UnresolvedThreads
		}
	}
	return w
}

func (r Runtime) journal() *store.MergeJournal {
	return store.NewMergeJournal(r.DB, func() (authority.Policy, error) { p, _, _, e := r.Policy(); return p, e })
}
func expected(c publication.Command, f store.PublishedRecord) (mergegate.Expected, error) {
	var proof store.PublicationProof
	var effects []store.PublicationEvidence
	if authority.DecodeStrict(f.Proof, &proof) != nil || authority.DecodeStrict(f.Effects, &effects) != nil || len(effects) != 4 || proof.PublicationID != c.ID() {
		return mergegate.Expected{}, ErrMerge
	}
	return mergegate.Expected{Repository: c.Repository, PullRequest: proof.PullRequest, HeadSHA: proof.CommitSHA, BaseSHA: c.SourceSHA, TreeSHA: effects[0].Receipt.SHA, Branch: proof.Branch}, nil
}

// gateChange names the first difference between the queued gate and its
// pre-send re-observation (for a gate no longer ready, its blocker in wait
// state precedence), or "" when the fresh observation is the same ready
// gate. Every name is a fixed constant.
func gateChange(queued, current mergegate.Observation) string {
	switch {
	case !queued.Ready || queued.MergeAuthorized || len(queued.Reasons) != 0:
		return "gate-queued-invalid"
	case current.MergeAuthorized:
		return "gate-not-ready"
	case !current.Ready || len(current.Reasons) != 0:
		// The same precedence as the wait states names the real blocker.
		present := map[string]bool{}
		for _, reason := range current.Reasons {
			present[reason] = true
		}
		for _, w := range waitStates {
			if present[w.reason] {
				return "gate-" + w.reason
			}
		}
		return "gate-not-ready"
	case queued.Expected != current.Expected:
		return "gate-changed-expected"
	case queued.PolicySHA256 != current.PolicySHA256:
		return "gate-changed-policy"
	case queued.ChecksSHA256 != current.ChecksSHA256:
		return "gate-changed-checks"
	case queued.ReviewsSHA256 != current.ReviewsSHA256:
		return "gate-changed-reviews"
	}
	return ""
}

// StopError names the bounded stage that stopped a claimed merge run. The
// stage is diagnostic only: the journal, not this controller, decides whether
// the run sent nothing (store.StopMerge records "not-sent" only when, under the
// request's row lock, no merge effect exists or the only one is sealed
// not-sent). So a "publication-fence" stop whose not-sent seal failed, or an
// "effect-intent" stop whose intent commit landed, stays "uncertain" and is
// never retried. Exhausted is set when the journal recorded this not-sent
// request as the publication's last allowed attempt. It unwraps to ErrMerge.
type StopError struct {
	Stage     string
	Exhausted bool
}

func (e StopError) Error() string { return "merge stopped at " + e.Stage }
func (e StopError) Unwrap() error { return ErrMerge }

// AttemptsExhausted reports whether a stop ended the publication's last
// allowed merge attempt (store.MergeAttempts) without sending anything.
func AttemptsExhausted(err error) bool {
	var stop StopError
	return errors.As(err, &stop) && stop.Exhausted
}

func stopped(stage string) error { return StopError{Stage: stage} }

// stopStages is the closed set of stage names a StopError may carry.
var stopStages = map[string]bool{
	// Before the claim or before any merge effect was begun.
	"request": true, "claim": true, "publication-fence": true, "gate-policy": true, "gate-manifest": true,
	"gate-unavailable": true, "gate-deadline": true, "gate-queued-invalid": true, "gate-not-ready": true,
	"gate-native-merge-not-ready": true, "gate-native-protection-insufficient": true,
	"gate-checks-not-passed": true, "gate-review-changes-requested": true, "gate-review-threads-unresolved": true,
	"gate-changed-expected": true, "gate-changed-policy": true, "gate-changed-checks": true,
	"gate-changed-reviews": true, "effect-intent": true, "transport-before-send": true,
	// After the merge request may have been sent.
	"transport": true, "sent-fence": true, "acknowledge": true, "finish": true,
}

// DiagnosticStage is bounded controller metadata for the stop log, for example
// "gate-changed-checks" or "transport". It never carries a response body, URL,
// credential or forge text; anything outside the closed set is "unknown".
func DiagnosticStage(err error) string {
	var stop StopError
	if !errors.As(err, &stop) || !stopStages[stop.Stage] {
		return "unknown"
	}
	return stop.Stage
}

// PollOnce is called only by the owning component loop. Waiting for CI does not
// reserve or extend a lease. An uncertain write blocks the component, and a
// publication whose request may have been sent is never turned into a fresh
// request. Only a request the journal recorded as "not-sent" (stopped before
// any merge effect was begun, or with its only effect sealed not-sent) leaves
// its publication selectable again, for a bounded number of attempts and only
// after the stopped command has expired (store.NextMergePublication). Each
// attempt observes the gate afresh and signs a new command.
func (r Runtime) PollOnce(ctx context.Context) (string, error) {
	if r.DB == nil || r.Policy == nil || r.Review == nil || r.Identity == nil || r.GatePolicy == nil || r.Gates == nil || r.Writer == nil {
		return "", ErrMerge
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	j := r.journal()
	if j.ExpireMerges(ctx) != nil {
		return "", ErrMerge
	}
	blocked, e := j.MergeBlocked(ctx)
	if e != nil {
		return "", ErrMerge
	}
	if blocked {
		if r.Reconciler == nil || r.Observer == nil {
			return "merge-quarantined", ErrMerge
		}
		// The failed request stays failed: reconciliation only records what the
		// forge shows. The quarantine lifts once no uncertain effect remains;
		// the next poll then selects work as usual.
		if _, _, e = r.Reconciler.Step(ctx, r.Observer); e != nil {
			return "merge-quarantined", ErrMerge
		}
		if blocked, e = j.MergeBlocked(ctx); e != nil || blocked {
			return "merge-quarantined", ErrMerge
		}
		return "merge-reconciled", nil
	}
	id, e := j.NextMerge(ctx)
	if e != nil {
		return "", ErrMerge
	}
	if id == "" {
		p, intake, profile, e := r.Policy()
		if e != nil {
			return "", ErrMerge
		}
		review, e := r.Review()
		if e != nil {
			return "", ErrMerge
		}
		publicationID, e := r.DB.NextRotatedMergePublication(ctx, store.MergeSelection{AuthorityRevision: p.Revision(), IntakeConfiguration: intake.Digest(), ExecutionProfile: profile, ReviewProfile: review.Profile, ReviewConfiguration: review.Configuration, RecipeRevision: verification.CurrentRecipeRevision(p.Repository)}, r.Rotation.snapshot())
		if e != nil {
			return "", ErrMerge
		}
		if publicationID == "" {
			return "", nil
		}
		// Every selected publication goes to the back of the rotation,
		// whatever this poll finds, so the next poll offers the next one.
		// The rotation also keeps the publication's latest wait (cleared by
		// a gate observation that is not a wait, kept when the gate could
		// not be observed), so wait metrics do not depend on which
		// publication or request one poll happened to handle.
		park := time.Duration(0)
		var waited *Wait
		gateObserved := false
		defer func() { r.Rotation.observed(publicationID, park, gateObserved, waited) }()
		c, fact, e := publicationflow.CurrentPublishedProof(ctx, r.DB, r.Policy, r.Review, publicationID)
		if e != nil {
			return "merge-authority-denied", ErrMerge
		}
		want, e := expected(c, fact)
		if e != nil {
			return "", ErrMerge
		}
		// The gate's merge settings come from the manifest the policy that
		// authorized the publication binds, never another policy load's.
		if p.Revision() != c.AuthorityRevision {
			return "", ErrMerge
		}
		gp, e := r.GatePolicy(p.ProjectSHA256)
		if errors.Is(e, mergegate.ErrManifest) {
			return "merge-gate-manifest", stopped("gate-manifest")
		}
		if e != nil {
			return "", ErrMerge
		}
		observed, e := r.Gates.Collect(ctx, want, gp)
		switch {
		case errors.Is(e, mergegate.ErrClosed):
			gateObserved = true
			// An observation, not a fault: the publication is parked so it
			// does not hold the loop, and is observed again after
			// ParkClosed in case the pull request is reopened.
			park = ParkClosed
			return "merge-pull-request-closed", nil
		case errors.Is(e, mergegate.ErrMergedOutside):
			gateObserved = true
			park = ParkClosed
			return "merge-pull-request-merged-outside", nil
		case e != nil:
			return "merge-observation-unavailable", ErrMerge
		}
		gateObserved = true
		if !observed.Ready {
			wait := waitFor(observed)
			waited = &wait
			if r.Waiting != nil {
				r.Waiting(wait)
			}
			return wait.State, nil
		}
		current, currentFact, e := publicationflow.CurrentPublishedProof(ctx, r.DB, r.Policy, r.Review, publicationID)
		if e != nil || current != c || currentFact.Digest() != fact.Digest() {
			return "", ErrMerge
		}
		p, _, _, e = r.Policy()
		if e != nil || p.Revision() != c.AuthorityRevision {
			return "", ErrMerge
		}
		key, e := r.Identity()
		if e != nil || key.Role != "release-manager" {
			return "", ErrMerge
		}
		gate, _ := json.Marshal(observed)
		now := time.Now()
		expires := observed.ObservedAt.Add(25 * time.Second)
		if !expires.After(now.Add(5 * time.Second)) {
			return "merge-observation-expired", ErrMerge
		}
		command := merging.Command{Version: merging.Version, PublicationID: publicationID, PublicationRecordSHA256: fact.Digest(), GateSHA256: authority.Digest(gate), Repository: c.Repository, IssueNumber: c.IssueNumber, PullRequest: want.PullRequest, HeadSHA: want.HeadSHA, BaseSHA: want.BaseSHA, TreeSHA: want.TreeSHA, Branch: want.Branch, AuthorityRevision: p.Revision(), IssuedAt: now.UnixMilli(), ExpiresAt: expires.UnixMilli()}
		payload, _ := json.Marshal(command)
		receipt, e := key.Seal(p, command.ID(), "merge-publication", payload)
		if e != nil {
			return "", ErrMerge
		}
		if _, e = merging.Verify(p, receipt, payload, now); e != nil {
			return "", ErrMerge
		}
		signed, _ := json.Marshal(receipt)
		request := store.MergeRequest{ID: command.ID(), PublicationID: publicationID, RecordDigest: fact.Digest(), GateDigest: command.GateSHA256, AuthorityRevision: p.Revision(), PayloadDigest: authority.Digest(payload), Payload: payload, Receipt: signed, Gate: gate, ExpiresAt: time.UnixMilli(command.ExpiresAt)}
		if j.QueueMerge(ctx, request) != nil {
			return "merge-queue-denied", ErrMerge
		}
		id = command.ID()
	}
	if e = r.run(ctx, j, id); e != nil {
		var stop StopError
		if errors.As(e, &stop) {
			if exhausted, err := j.MergeAttemptsExhausted(ctx, id); err == nil && exhausted {
				stop.Exhausted = true
				e = stop
			}
		}
		return "merge-stopped", e
	}
	return "merge-complete", nil
}
func (r Runtime) run(ctx context.Context, j *store.MergeJournal, id string) (err error) {
	request, e := j.ReadMerge(ctx, id)
	if e != nil {
		return stopped("request")
	}
	p, _, _, e := r.Policy()
	if e != nil {
		return stopped("request")
	}
	var signed authority.Receipt
	if authority.DecodeStrict(request.Receipt, &signed) != nil {
		return stopped("request")
	}
	command, e := merging.Verify(p, signed, request.Payload, time.Now())
	if e != nil || command.ID() != id {
		return stopped("request")
	}
	var gate mergegate.Observation
	if authority.DecodeStrict(request.Gate, &gate) != nil {
		return stopped("request")
	}
	claim, e := j.ClaimMerge(ctx, id)
	if e != nil {
		return stopped("claim")
	}
	ctx, cancel := context.WithDeadline(ctx, claim.Until)
	defer cancel()
	defer func() {
		if recover() != nil {
			err = stopped("transport")
		}
		if err != nil {
			// The journal records "not-sent" instead when, under the claim
			// lock, no merge effect was begun or the only one is sealed
			// not-sent; any other stop stays uncertain.
			cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			_ = j.StopMerge(cleanup, claim, "uncertain")
		}
	}()
	bound := func(ctx context.Context, c publication.Command, fact store.PublishedRecord, e error) error {
		if e != nil || fact.Digest() != command.PublicationRecordSHA256 || c.AuthorityRevision != command.AuthorityRevision {
			return ErrMerge
		}
		want, e := expected(c, fact)
		if e != nil || want != gate.Expected || c.IssueNumber != command.IssueNumber {
			return ErrMerge
		}
		return j.CheckMerge(ctx, claim)
	}
	// fence guards everything up to and including the last pre-send check: the
	// issue must still be the same open, eligible generation.
	fence := func(ctx context.Context) error {
		c, fact, e := publicationflow.CurrentPublishedProof(ctx, r.DB, r.Policy, r.Review, command.PublicationID)
		return bound(ctx, c, fact, e)
	}
	// sent guards the stages after the merge request may have been sent: the
	// writer's post-response check, the acknowledgement and the finish. For a
	// manifest repository the pull request says "Closes #N", so the merge itself
	// closes the issue and the intake records a new generation. Like the other
	// post-effect stages, recording the outcome binds to the claim, lease,
	// authority, immutable publication fact, gate, request digest and receipt,
	// not to issue state the effect itself changes.
	sent := func(ctx context.Context) error {
		c, fact, e := publicationflow.SentPublishedProof(ctx, r.DB, r.Policy, r.Review, command.PublicationID)
		return bound(ctx, c, fact, e)
	}
	var effect store.MergeEffect
	calls, armed := 0, false
	// presend holds the stage that refused the first fence call, so the stop
	// names why no merge request was sent.
	presend := ""
	transportFence := func(ctx context.Context, digest string) error {
		calls++
		if calls > 2 {
			return ErrMerge
		}
		if calls == 1 {
			deny := func(stage string) error { presend = stage; return ErrMerge }
			if fence(ctx) != nil {
				return deny("publication-fence")
			}
			authorizing, _, _, e := r.Policy()
			if e != nil || authorizing.Revision() != command.AuthorityRevision {
				return deny("publication-fence")
			}
			gp, e := r.GatePolicy(authorizing.ProjectSHA256)
			if errors.Is(e, mergegate.ErrManifest) {
				return deny("gate-manifest")
			}
			if e != nil {
				return deny("gate-policy")
			}
			current, e := r.Gates.Collect(ctx, gate.Expected, gp)
			if e != nil {
				if ctx.Err() != nil {
					// The claim (capped by the gate's lifetime) ended while
					// the gate was being observed again.
					return deny("gate-deadline")
				}
				return deny("gate-unavailable")
			}
			if change := gateChange(gate, current); change != "" {
				return deny(change)
			}
			if fence(ctx) != nil {
				return deny("publication-fence")
			}
			effect, e = j.BeginMergeEffect(ctx, claim, digest)
			if e != nil {
				return deny("effect-intent")
			}
			if fence(ctx) != nil {
				cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
				defer stop()
				_ = j.SealMergeNotSent(cleanup, effect)
				return deny("publication-fence")
			}
			armed = true
			return nil
		}
		// The writer's second call follows the merge response.
		if !armed || effect.RequestDigest != digest || sent(ctx) != nil {
			return ErrMerge
		}
		return nil
	}
	receipt, e := r.Writer.Merge(ctx, gate.Expected, command.IssueNumber, transportFence)
	if e != nil || calls != 2 || effect.Token == "" {
		switch {
		case presend != "":
			return stopped(presend)
		case !armed:
			return stopped("transport-before-send")
		}
		return stopped("transport")
	}
	if sent(ctx) != nil {
		return stopped("sent-fence")
	}
	stored := store.MergeReceipt{Version: receipt.Version, CommitSHA: receipt.CommitSHA, HeadSHA: receipt.HeadSHA, BaseSHA: receipt.BaseSHA, TreeSHA: receipt.TreeSHA, PullRequest: receipt.PullRequest}
	if j.AcknowledgeMergeEffect(ctx, effect, stored) != nil {
		return stopped("acknowledge")
	}
	if sent(ctx) != nil {
		return stopped("sent-fence")
	}
	evidence, e := j.ReadMergeEvidence(ctx, claim)
	if e != nil {
		return stopped("finish")
	}
	raw, _ := json.Marshal(evidence)
	proof := store.MergeProof{Version: "ai-sdlc.merge-proof/v1", MergeID: id, PublicationID: command.PublicationID, PublicationRecordSHA256: claim.RecordDigest, GateSHA256: claim.GateDigest, EffectSHA256: authority.Digest(raw), Receipt: stored}
	raw, _ = json.Marshal(proof)
	if sent(ctx) != nil {
		return stopped("sent-fence")
	}
	if j.FinishMerge(ctx, claim, raw) != nil {
		return stopped("finish")
	}
	return nil
}
