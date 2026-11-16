package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
)

// HandoffCandidate is called only by the installed worker reconciler after
// acknowledged cleanup. It performs no model, forge or Kubernetes operation.
// Its fixed recipe is selected by current signed issue evidence, not a caller.
func HandoffCandidate(ctx context.Context, db *store.DB, load func() (authority.Policy, authority.Identity, intake.Config, string, error)) (bool, error) {
	p, key, config, profile, err := load()
	if err != nil || key.Role != "worker" {
		return false, ErrAuthority
	}
	id, err := db.NextCandidateHandoff(ctx, p.Revision())
	if err != nil || id == "" {
		return false, err
	}
	record, err := db.ReadCapturedCandidate(ctx, id)
	if err != nil {
		return false, err
	}
	var receipt authority.Receipt
	if authority.DecodeStrict(record.Receipt, &receipt) != nil {
		return false, ErrAuthority
	}
	command, err := Verify(p, receipt, record.Payload, time.Now())
	if err != nil || command.ProfileSHA256 != profile || command.ID() != id || command.WorkItemID != record.WorkItem || command.Repository != record.Repository || command.SourceSHA != record.Record.SourceSHA {
		_ = db.RejectCandidateHandoff(ctx, id, "revoked")
		return false, ErrAuthority
	}
	snapshot, err := verifiedSnapshotStatus(ctx, db, p, config, command, "running")
	if err != nil {
		if errors.Is(err, ErrAuthority) {
			_ = db.RejectCandidateHandoff(ctx, id, "revoked")
		}
		return false, err
	}
	// Marker parsing is deliberately finite. Full reconstruction remains the
	// independent verifier's responsibility, never the generator's assertion.
	recipe, selected := verification.SelectRecipe(command.Repository, snapshot.Issue.Body)
	if !selected {
		_ = db.RejectCandidateHandoff(ctx, id, "unsupported-recipe")
		return false, nil
	}
	expires := min(command.ExpiresAt, time.Now().Add(verification.MaxCommandAge).UnixMilli())
	c := verification.Command{Version: verification.CommandVersion, CandidateID: id, WorkItemID: record.WorkItem, Repository: record.Repository, IssueNumber: command.IssueNumber, Generation: command.Generation, IssueRevision: snapshot.Revision, SourceSHA: command.SourceSHA, DeltaSHA256: record.Record.DeltaSHA256, CandidateTreeSHA256: record.Record.CandidateTreeSHA256, Recipe: recipe, RecipeRevision: verification.RevisionForRecipe(recipe), AuthorityRevision: p.Revision(), ExpiresAt: expires}
	if verification.MatchLedger(c, record) != nil {
		return false, ErrAuthority
	}
	raw, _ := json.Marshal(c)
	current, currentKey, currentConfig, currentProfile, err := load()
	if err != nil || currentProfile != profile || current.Revision() != p.Revision() || currentConfig.Digest() != config.Digest() || currentKey.KeyID != key.KeyID {
		return false, ErrAuthority
	}
	signed, err := currentKey.Seal(current, c.ID(), "verify-artifact", raw)
	if err != nil {
		return false, ErrAuthority
	}
	if _, err = verification.Verify(current, signed, raw, time.Now()); err != nil {
		return false, err
	}
	err = db.WithTx(ctx, func(tx *sql.Tx) error {
		if e := store.RecordCandidateHandoff(ctx, tx, id, c.ID(), authority.Digest(raw)); e != nil {
			return e
		}
		return delivery.Append(ctx, tx, current, currentKey, c.ID(), "verify-artifact", raw)
	})
	return err == nil, err
}
