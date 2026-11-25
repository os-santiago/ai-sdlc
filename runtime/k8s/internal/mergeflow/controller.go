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
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergewriter"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/merging"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publication"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publicationflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
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
	GatePolicy func() (mergegate.Policy, error)
	Gates      Gates
	Writer     Transport
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
func sameGate(a, b mergegate.Observation) bool {
	return a.Ready && b.Ready && !a.MergeAuthorized && !b.MergeAuthorized && len(a.Reasons) == 0 && len(b.Reasons) == 0 && a.Expected == b.Expected && a.PolicySHA256 == b.PolicySHA256 && a.ChecksSHA256 == b.ChecksSHA256 && a.ReviewsSHA256 == b.ReviewsSHA256
}

// PollOnce is called only by the owning component loop. Waiting for CI does not
// reserve or extend a lease. An uncertain write blocks the component, and an
// already-attempted publication is never turned into a fresh request.
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
	if e != nil || blocked {
		return "merge-quarantined", ErrMerge
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
		publicationID, e := r.DB.NextMergePublication(ctx, store.MergeSelection{AuthorityRevision: p.Revision(), IntakeConfiguration: intake.Digest(), ExecutionProfile: profile, ReviewProfile: review.Profile, ReviewConfiguration: review.Configuration})
		if e != nil {
			return "", ErrMerge
		}
		if publicationID == "" {
			return "", nil
		}
		c, fact, e := publicationflow.CurrentPublishedProof(ctx, r.DB, r.Policy, r.Review, publicationID)
		if e != nil {
			return "merge-authority-denied", ErrMerge
		}
		want, e := expected(c, fact)
		if e != nil {
			return "", ErrMerge
		}
		gp, e := r.GatePolicy()
		if e != nil {
			return "", ErrMerge
		}
		observed, e := r.Gates.Collect(ctx, want, gp)
		if e != nil {
			return "merge-observation-unavailable", ErrMerge
		}
		if !observed.Ready {
			return "merge-waiting-for-checks", nil
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
		return "merge-stopped", e
	}
	return "merge-complete", nil
}
func (r Runtime) run(ctx context.Context, j *store.MergeJournal, id string) (err error) {
	request, e := j.ReadMerge(ctx, id)
	if e != nil {
		return ErrMerge
	}
	p, _, _, e := r.Policy()
	if e != nil {
		return ErrMerge
	}
	var signed authority.Receipt
	if authority.DecodeStrict(request.Receipt, &signed) != nil {
		return ErrMerge
	}
	command, e := merging.Verify(p, signed, request.Payload, time.Now())
	if e != nil || command.ID() != id {
		return ErrMerge
	}
	var gate mergegate.Observation
	if authority.DecodeStrict(request.Gate, &gate) != nil {
		return ErrMerge
	}
	claim, e := j.ClaimMerge(ctx, id)
	if e != nil {
		return ErrMerge
	}
	ctx, cancel := context.WithDeadline(ctx, claim.Until)
	defer cancel()
	defer func() {
		if recover() != nil {
			err = ErrMerge
		}
		if err != nil {
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
	transportFence := func(ctx context.Context, digest string) error {
		calls++
		if calls > 2 {
			return ErrMerge
		}
		if calls == 1 {
			if fence(ctx) != nil {
				return ErrMerge
			}
			gp, e := r.GatePolicy()
			if e != nil {
				return ErrMerge
			}
			current, e := r.Gates.Collect(ctx, gate.Expected, gp)
			if e != nil || !sameGate(gate, current) {
				return ErrMerge
			}
			if fence(ctx) != nil {
				return ErrMerge
			}
			effect, e = j.BeginMergeEffect(ctx, claim, digest)
			if e != nil {
				return ErrMerge
			}
			if fence(ctx) != nil {
				cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
				defer stop()
				_ = j.SealMergeNotSent(cleanup, effect)
				return ErrMerge
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
	if e != nil || calls != 2 || effect.Token == "" || sent(ctx) != nil {
		return ErrMerge
	}
	stored := store.MergeReceipt{Version: receipt.Version, CommitSHA: receipt.CommitSHA, HeadSHA: receipt.HeadSHA, BaseSHA: receipt.BaseSHA, TreeSHA: receipt.TreeSHA, PullRequest: receipt.PullRequest}
	if j.AcknowledgeMergeEffect(ctx, effect, stored) != nil || sent(ctx) != nil {
		return ErrMerge
	}
	evidence, e := j.ReadMergeEvidence(ctx, claim)
	if e != nil {
		return ErrMerge
	}
	raw, _ := json.Marshal(evidence)
	proof := store.MergeProof{Version: "ai-sdlc.merge-proof/v1", MergeID: id, PublicationID: command.PublicationID, PublicationRecordSHA256: claim.RecordDigest, GateSHA256: claim.GateDigest, EffectSHA256: authority.Digest(raw), Receipt: stored}
	raw, _ = json.Marshal(proof)
	if sent(ctx) != nil || j.FinishMerge(ctx, claim, raw) != nil {
		return ErrMerge
	}
	return nil
}
