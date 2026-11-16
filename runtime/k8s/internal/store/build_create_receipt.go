package store

import (
	"context"
	"database/sql"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"time"
)

// RememberBuildCreated records only an original POST response for custody and
// cleanup, even after freeze or expiry. It cannot claim, resume, publish or finish
// work. Every execution transition still requires current policy and freshness.
func (j *BuildJournal) RememberBuildCreated(ctx context.Context, effect BuildEffect, uid string) error {
	c := effect.Claim
	if j == nil || j.DB == nil || !buildIDPattern.MatchString(c.ID) || !lifecycleUUID.MatchString(c.Token) || !lifecycleUUID.MatchString(effect.Token) || !lifecycleUUID.MatchString(uid) || (effect.Kind != "build" && effect.Kind != "publish") || !modelDigest.MatchString(effect.RequestDigest) {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "build-controller") != nil {
			return ErrExecution
		}
		var payload, receipt, inner, outer, slotRaw []byte
		if tx.QueryRowContext(ctx, `SELECT r.payload,r.receipt,r.inner_policy,r.outer_policy,s.slot FROM build_requests r JOIN build_slots s ON s.request_id=r.id WHERE r.id=$1 AND r.claim_token=$2 AND s.slot_id=$3 AND s.slot_digest=$4 AND r.state IN ('claimed','failed') FOR UPDATE OF r`, c.ID, c.Token, c.SlotID, c.SlotDigest).Scan(&payload, &receipt, &inner, &outer, &slotRaw) != nil {
			return ErrExecution
		}
		// A separate statement obtains a fresh Read Committed snapshot AFTER
		// acquiring the shared cleanup/ACK row lock, including after a wait.
		var closed bool
		if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM build_cleanups WHERE request_id=$1)`, c.ID).Scan(&closed) != nil || closed {
			return ErrExecution
		}
		cmd, e := archivedBuild(payload, receipt, inner, outer)
		if e != nil || cmd.ID() != c.ID {
			return ErrExecution
		}
		var slot building.Slot
		if authority.DecodeStrict(slotRaw, &slot) != nil || slot.Validate() != nil || slot.ID() != c.SlotID || slot.Digest() != c.SlotDigest {
			return ErrExecution
		}
		plan, e := readBuildPlan(ctx, tx, c.ID)
		if e != nil || plan.Validate(cmd, slot) != nil {
			return ErrExecution
		}
		wanted := plan.BuildRequestSHA256
		if effect.Kind == "publish" {
			wanted = plan.PublishRequestSHA256
		}
		if effect.RequestDigest != wanted || effect.Name != "ai-sdlc-"+effect.Kind+"-"+c.ID[len("build-"):len("build-")+32] {
			return ErrExecution
		}
		result, e := tx.ExecContext(ctx, `UPDATE build_effects SET resource_uid=$6,acknowledged_at=clock_timestamp() WHERE request_id=$1 AND kind=$2 AND token=$3 AND request_digest=$4 AND resource_name=$5 AND resource_uid IS NULL`, c.ID, effect.Kind, effect.Token, effect.RequestDigest, effect.Name, uid)
		if e != nil {
			return ErrExecution
		}
		n, e := result.RowsAffected()
		if e != nil || n != 1 {
			return ErrExecution
		}
		return nil
	})
}
