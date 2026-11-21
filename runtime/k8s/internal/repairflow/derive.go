// Package repairflow connects authenticated reviewer rejection to one bounded
// orchestrator derivation. It never invokes Kubernetes, GitHub or a model.
package repairflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repair"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
)

type Inputs struct {
	Policy                             authority.Policy
	Identity                           authority.Identity
	Intake                             intake.Config
	Runtime                            execution.RuntimeConfig
	Profile                            []byte
	ReviewProfile, ReviewConfiguration string
}
type Loader func(context.Context) (Inputs, error)

func validInputs(in Inputs, role string) bool {
	profile, e := executor.ParseProfile(in.Profile, authority.Digest(in.Profile))
	raw, _ := json.Marshal(in.Runtime.Profile)
	return e == nil && profile.MaxRepairs == 1 && in.Identity.Role == role && in.Intake.Validate(in.Policy.Repository) == nil &&
		authority.Digest(raw) == authority.Digest(in.Profile) && authority.Digest([]byte(in.Runtime.ADev)) == profile.ADevSHA256
}

func sameInputs(a, b Inputs) bool {
	x, _ := json.Marshal(a.Runtime)
	y, _ := json.Marshal(b.Runtime)
	return a.Policy.Revision() == b.Policy.Revision() && a.Identity.KeyID == b.Identity.KeyID && a.Identity.Role == b.Identity.Role &&
		a.Intake.Digest() == b.Intake.Digest() && authority.Digest(a.Profile) == authority.Digest(b.Profile) && authority.Digest(x) == authority.Digest(y) &&
		a.ReviewProfile == b.ReviewProfile && a.ReviewConfiguration == b.ReviewConfiguration
}

func feedbackReference(f store.ReviewFeedbackRecord, sourceTree string) repair.Reference {
	d := f.Decision
	return repair.Reference{ParentCandidateID: d.CandidateID, ParentReviewID: d.ReviewID, VerificationID: d.VerificationID,
		FeedbackSHA256: f.Digest, BehaviorSHA256: d.BehaviorSHA256, ParentTreeSHA256: d.CandidateTreeSHA256, OriginalTreeSHA256: sourceTree}
}

// Effect is reachable only through the signed orchestrator inbox. It validates
// the original signed generation and verification, then locks unused capacity.
func Effect(load Loader) delivery.Effect {
	return func(ctx context.Context, tx *sql.Tx, receipt authority.Receipt, raw []byte) ([]byte, error) {
		if load == nil {
			return nil, repair.ErrRepair
		}
		in, e := load(ctx)
		if e != nil || !validInputs(in, "orchestrator") {
			return nil, repair.ErrRepair
		}
		c, e := repair.Verify(in.Policy, receipt, raw, time.Now())
		if e != nil {
			return nil, e
		}
		f, e := store.ReadRepairFeedback(ctx, tx, c.Reference.ParentReviewID)
		if e != nil {
			return nil, e
		}
		parent, e := store.ReadRepairCandidate(ctx, tx, c.Reference.ParentCandidateID)
		if e != nil {
			return nil, e
		}
		if feedbackReference(f, parent.Record.SourceTreeSHA256) != c.Reference || f.Decision.WorkItemID != c.WorkItemID ||
			f.Decision.ProfileSHA256 != in.ReviewProfile || f.Decision.ConfigurationSHA256 != in.ReviewConfiguration {
			return nil, repair.ErrRepair
		}
		var signed authority.Receipt
		if authority.DecodeStrict(parent.Receipt, &signed) != nil {
			return nil, repair.ErrRepair
		}
		original, e := execution.Verify(in.Policy, signed, parent.Payload, time.Now())
		if e != nil || original.Attempt != 1 || original.Repair != nil || original.ID() != c.Reference.ParentCandidateID ||
			original.WorkItemID != c.WorkItemID || parent.WorkItem != c.WorkItemID || original.ProfileSHA256 != authority.Digest(in.Profile) || c.ExpiresAt > original.ExpiresAt {
			return nil, repair.ErrRepair
		}
		v, e := store.ReadRepairVerification(ctx, tx, c.Reference.VerificationID)
		if e != nil {
			return nil, e
		}
		if authority.DecodeStrict(v.Receipt, &signed) != nil || authority.Digest(v.Payload) != v.PayloadDigest {
			return nil, repair.ErrRepair
		}
		verified, e := verification.Verify(in.Policy, signed, v.Payload, time.Now())
		if e != nil || verified.ID() != c.Reference.VerificationID || verification.MatchLedger(verified, parent) != nil ||
			verified.WorkItemID != original.WorkItemID || verified.Generation != original.Generation || verified.IssueNumber != original.IssueNumber ||
			verified.SourceSHA != original.SourceSHA || verified.ExpiresAt > original.ExpiresAt || c.ExpiresAt > verified.ExpiresAt {
			return nil, repair.ErrRepair
		}
		behavior, e := store.ReadRepairBehavior(ctx, tx, v.ID)
		var proof verificationflow.BehaviorProof
		if e != nil || authority.Digest(behavior) != c.Reference.BehaviorSHA256 || authority.DecodeStrict(behavior, &proof) != nil ||
			!verificationflow.ValidBehaviorProof(proof, verified) {
			return nil, repair.ErrRepair
		}
		allowance, e := store.LockRepairParent(ctx, tx, c.WorkItemID, original.ID(), parent.Record.AllocationID)
		if e != nil {
			return nil, e
		}
		model, _ := json.Marshal(in.Runtime.Model)
		if allowance.ConfigurationDigest != authority.Digest(model) || allowance.OriginalLimits != in.Runtime.Model.Limits || allowance.ResponseLimit != in.Runtime.Model.ResponseLimit {
			return nil, repair.ErrRepair
		}
		allowance.ExpiresAt = time.UnixMilli(min(allowance.ExpiresAt.UnixMilli(), c.ExpiresAt))
		snapshot, e := execution.RepairSnapshot(ctx, tx, in.Policy, in.Intake, original)
		if e != nil || snapshot.Revision != verified.IssueRevision {
			return nil, repair.ErrRepair
		}
		child := original
		child.Attempt = 2
		child.Repair = &c.Reference
		child.ExpiresAt = allowance.ExpiresAt.UnixMilli()
		payload, _ := json.Marshal(child)
		if e = store.InsertRepairReservation(ctx, tx, c.WorkItemID, c.Reference.ParentReviewID, original.ID(), parent.Record.AllocationID, child.ID(), store.NewID(), authority.Digest(raw), payload, allowance); e != nil {
			return nil, e
		}
		latest, e := load(ctx)
		if e != nil || !sameInputs(in, latest) || !validInputs(latest, "orchestrator") {
			return nil, repair.ErrRepair
		}
		if e = delivery.Append(ctx, tx, latest.Policy, latest.Identity, child.ID(), "execute-work", payload); e != nil {
			return nil, e
		}
		// All unique/FK/outbox waits precede the final issue, policy and clock checks.
		if e = store.CheckRepairReservation(ctx, tx, c.WorkItemID, child.ID(), payload); e != nil {
			return nil, e
		}
		if _, e = execution.RepairSnapshot(ctx, tx, latest.Policy, latest.Intake, original); e != nil {
			return nil, e
		}
		final, e := load(ctx)
		if e != nil || !sameInputs(in, final) || !time.UnixMilli(child.ExpiresAt).After(time.Now()) {
			return nil, repair.ErrRepair
		}
		if _, e = repair.Verify(final.Policy, receipt, raw, time.Now()); e != nil {
			return nil, e
		}
		return json.Marshal(map[string]string{"executionID": child.ID(), "state": "reserved"})
	}
}
