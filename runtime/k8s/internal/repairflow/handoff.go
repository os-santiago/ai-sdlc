package repairflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repair"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
)

// Handoff runs only in the verifier's bounded reconciler, after cleanup. A
// second rejection and ineligible history are terminal, never review shopping.
func Handoff(ctx context.Context, db *store.DB, load Loader) (bool, error) {
	if db == nil || load == nil {
		return false, repair.ErrRepair
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	in, e := load(ctx)
	if e != nil || in.Identity.Role != "verifier" {
		return false, repair.ErrRepair
	}
	stopID, reason, child, e := db.NextRejectedStop(ctx)
	if e != nil {
		return false, e
	}
	if stopID != "" {
		f, e := db.ReadReviewFeedback(ctx, stopID)
		if e != nil {
			return false, e
		}
		_, e = stopHandoff(ctx, db, load, in, f, reason, child, false)
		return false, e
	}
	id, e := db.NextRepairFeedback(ctx)
	if e != nil || id == "" {
		return false, e
	}
	f, e := db.ReadReviewFeedback(ctx, id)
	if e != nil {
		return false, e
	}
	terminal := func(reason string) (bool, error) {
		_, e := stopHandoff(ctx, db, load, in, f, reason, "", true)
		return false, e
	}
	profile, e := executor.ParseProfile(in.Profile, authority.Digest(in.Profile))
	if e != nil {
		return false, repair.ErrRepair
	}
	if profile.MaxRepairs != 1 {
		return terminal("not-authorized")
	}
	parent, e := db.ReadVerificationCandidate(ctx, f.Decision.CandidateID)
	if e != nil {
		return false, e
	}
	var original execution.Command
	if authority.DecodeStrict(parent.Payload, &original) != nil {
		return terminal("stale")
	}
	if original.Attempt != 1 || original.Repair != nil {
		return terminal("terminal-rejection")
	}
	if !time.UnixMilli(original.ExpiresAt).After(time.Now()) {
		return terminal("expired")
	}
	if original.ProfileSHA256 != authority.Digest(in.Profile) || f.Decision.ProfileSHA256 != in.ReviewProfile || f.Decision.ConfigurationSHA256 != in.ReviewConfiguration {
		return terminal("stale")
	}
	policy := func() (authority.Policy, intake.Config, string, error) {
		current, e := load(ctx)
		if e != nil || !sameInputs(in, current) || !validInputs(current, "verifier") {
			return authority.Policy{}, intake.Config{}, "", repair.ErrRepair
		}
		return current.Policy, current.Intake, authority.Digest(current.Profile), nil
	}
	v, record, _, e := verificationflow.CurrentProof(ctx, db, policy, f.Decision.VerificationID)
	if e != nil {
		if errors.Is(e, store.ErrObservationUnavailable) {
			return false, e
		}
		return terminal("stale")
	}
	if v.CandidateID != parent.Record.RequestID || record.Record != parent.Record || v.CandidateTreeSHA256 != f.Decision.CandidateTreeSHA256 {
		return terminal("stale")
	}
	allowance, e := db.ReadRepairAllowance(ctx, original.WorkItemID, original.ID(), parent.Record.AllocationID)
	if e != nil {
		if errors.Is(e, store.ErrRepairExhausted) {
			return terminal("exhausted")
		}
		if errors.Is(e, store.ErrRepairExpired) {
			return terminal("expired")
		}
		return false, e
	}
	c := repair.Command{Version: repair.Version, Repository: original.Repository, WorkItemID: original.WorkItemID,
		AuthorityRevision: in.Policy.Revision(), Reference: feedbackReference(f, parent.Record.SourceTreeSHA256),
		ExpiresAt: min(original.ExpiresAt, v.ExpiresAt, allowance.ExpiresAt.UnixMilli())}
	raw, _ := json.Marshal(c)
	signed, e := in.Identity.Seal(in.Policy, c.ID(), "repair-candidate", raw)
	if e != nil {
		return false, repair.ErrRepair
	}
	if _, e = repair.Verify(in.Policy, signed, raw, time.Now()); e != nil {
		return false, e
	}
	e = db.WithTx(ctx, func(tx *sql.Tx) error {
		if e := store.RecordRepairHandoff(ctx, tx, id, c.WorkItemID, f.Digest, "forwarded", raw); e != nil {
			return e
		}
		if e := delivery.Append(ctx, tx, in.Policy, in.Identity, c.ID(), "repair-candidate", raw); e != nil {
			return e
		}
		// Recheck after the unique/outbox waits. The orchestrator performs its
		// own complete proof and locks capacity before deriving execution.
		current, _, _, e := verificationflow.CurrentProofTx(ctx, db, tx, policy, v.ID())
		if e != nil || current != v {
			return repair.ErrRepair
		}
		latest, e := load(ctx)
		if e != nil || !sameInputs(in, latest) {
			return repair.ErrRepair
		}
		_, e = repair.Verify(latest.Policy, signed, raw, time.Now())
		return e
	})
	return e == nil, e
}
