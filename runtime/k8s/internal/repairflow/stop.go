package repairflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repair"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func stopHandoff(ctx context.Context, db *store.DB, load Loader, in Inputs, f store.ReviewFeedbackRecord, reason, child string, newOutcome bool) (bool, error) {
	s := repair.Stop{Version: "ai-sdlc.rejected-work/v1", Repository: in.Policy.Repository, WorkItemID: f.Decision.WorkItemID, ReviewID: f.Decision.ReviewID, FeedbackSHA256: f.Digest, Reason: reason, UnstartedChildID: child}
	raw, _ := json.Marshal(s)
	signed, e := in.Identity.Seal(in.Policy, s.ID(), "stop-rejected-work", raw)
	if e != nil {
		return false, repair.ErrRepair
	}
	e = db.WithTx(ctx, func(tx *sql.Tx) error {
		if newOutcome {
			if e := store.RecordRepairHandoff(ctx, tx, s.ReviewID, s.WorkItemID, s.FeedbackSHA256, reason, nil); e != nil {
				return e
			}
		}
		if e := store.RecordRejectedStop(ctx, tx, s, raw); e != nil {
			return e
		}
		if e := delivery.Append(ctx, tx, in.Policy, in.Identity, s.ID(), "stop-rejected-work", raw); e != nil {
			return e
		}
		latest, e := load(ctx)
		if e != nil || !sameInputs(in, latest) {
			return repair.ErrRepair
		}
		_, e = repair.VerifyStop(latest.Policy, signed, raw)
		return e
	})
	return e == nil, e
}

func StopEffect(load func() (authority.Policy, error)) delivery.Effect {
	return func(ctx context.Context, tx *sql.Tx, receipt authority.Receipt, raw []byte) ([]byte, error) {
		p, e := load()
		if e != nil {
			return nil, repair.ErrRepair
		}
		s, e := repair.VerifyStop(p, receipt, raw)
		if e != nil {
			return nil, e
		}
		f, e := store.ReadRepairFeedback(ctx, tx, s.ReviewID)
		if e != nil || f.Digest != s.FeedbackSHA256 || f.Decision.WorkItemID != s.WorkItemID {
			return nil, repair.ErrRepair
		}
		parent, e := store.ReadRepairCandidate(ctx, tx, f.Decision.CandidateID)
		if e != nil {
			return nil, e
		}
		var original authority.Receipt
		var c execution.Command
		// Authenticate historical generation without reopening its expiry or
		// requiring an obsolete policy revision to grant new execution.
		if authority.DecodeStrict(parent.Receipt, &original) != nil || p.VerifyReceipt(original, "worker", "execute-work", parent.Payload) != nil ||
			authority.DecodeStrict(parent.Payload, &c) != nil || c.Version != execution.Version || c.ID() != f.Decision.CandidateID || original.ID != c.ID() ||
			c.Repository != s.Repository || c.WorkItemID != s.WorkItemID || parent.WorkItem != s.WorkItemID || !f.RecordedAt.Before(time.UnixMilli(c.ExpiresAt)) {
			return nil, repair.ErrRepair
		}
		state, e := store.CloseRejectedWork(ctx, tx, s, raw, c.ID(), c.IssueNumber, c.Generation)
		if e != nil {
			return nil, e
		}
		latest, e := load()
		if e != nil || latest.Revision() != p.Revision() || ctx.Err() != nil {
			return nil, repair.ErrRepair
		}
		if state == "failed" {
			if e = store.CheckRejectedHead(ctx, tx, s.Repository, c.IssueNumber, c.Generation); e != nil {
				return nil, e
			}
		}
		if _, e = repair.VerifyStop(latest, receipt, raw); e != nil {
			return nil, e
		}
		return json.Marshal(map[string]string{"workItemID": s.WorkItemID, "state": state})
	}
}
