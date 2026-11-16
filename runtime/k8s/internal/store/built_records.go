package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
)

// BuiltRecord is historical evidence, not verified provenance or release authority.
type BuiltRecord struct {
	Command        building.Command
	Plan           building.Plan
	Build, Publish building.Termination
	Result         building.Result
	RecordedAt     time.Time
}

func (db *DB) ReadBuiltRecord(ctx context.Context, id string) (BuiltRecord, error) {
	return db.readBuiltRecord(ctx, id, "verifier")
}
func (db *DB) ReadOwnBuiltRecord(ctx context.Context, id string) (BuiltRecord, error) {
	return db.readBuiltRecord(ctx, id, "build-controller")
}
func archivedBuild(payload, receipt, inner, outer []byte) (building.Command, error) {
	var c building.Command
	var signed authority.Receipt
	var ip, op authority.Policy
	if authority.DecodeStrict(payload, &c) != nil || authority.DecodeStrict(receipt, &signed) != nil || authority.DecodeStrict(inner, &ip) != nil || authority.DecodeStrict(outer, &op) != nil {
		return c, ErrExecution
	}
	verified, e := building.Verify(ip, op, signed, payload, time.UnixMilli(c.IssuedAt))
	if e != nil {
		return building.Command{}, ErrExecution
	}
	return verified, nil
}
func (db *DB) readBuiltRecord(ctx context.Context, id, role string) (BuiltRecord, error) {
	var r BuiltRecord
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if !buildIDPattern.MatchString(id) || (role != "verifier" && role != "build-controller") || AssertComponent(ctx, db.Pool, role) != nil {
		return r, ErrExecution
	}
	var payload, receipt, inner, outer, raw, slotRaw []byte
	var digest, slotDigest string
	var expires time.Time
	if db.Pool.QueryRowContext(ctx, `SELECT q.payload,q.receipt,q.inner_policy,q.outer_policy,f.result,f.digest,f.recorded_at,s.slot,s.slot_digest,q.expires_at FROM built_records f JOIN build_requests q ON q.id=f.request_id JOIN build_slots s ON s.request_id=q.id WHERE q.id=$1 AND q.state='built' AND f.recorded_at>=q.created_at AND f.recorded_at<q.expires_at AND f.recorded_at<=clock_timestamp()`, id).Scan(&payload, &receipt, &inner, &outer, &raw, &digest, &r.RecordedAt, &slotRaw, &slotDigest, &expires) != nil {
		return BuiltRecord{}, ErrExecution
	}
	var e error
	r.Command, e = archivedBuild(payload, receipt, inner, outer)
	var slot building.Slot
	if e != nil || !expires.Equal(time.UnixMilli(r.Command.ExpiresAt)) || r.RecordedAt.Before(time.UnixMilli(r.Command.IssuedAt)) || !r.RecordedAt.Before(time.UnixMilli(r.Command.ExpiresAt)) || r.Command.ID() != id || authority.DecodeStrict(raw, &r.Result) != nil || authority.Digest(raw) != digest || authority.DecodeStrict(slotRaw, &slot) != nil || slot.Validate() != nil || slot.Digest() != slotDigest {
		return BuiltRecord{}, ErrExecution
	}
	r.Plan, e = readBuildPlan(ctx, db.Pool, id)
	if e != nil || r.Plan.Validate(r.Command, slot) != nil {
		return BuiltRecord{}, ErrExecution
	}
	var at time.Time
	r.Build, at, e = readBuildObservation(ctx, db.Pool, id, "build")
	if e != nil || r.Build.Validate(r.Command, r.Plan, slot, at) != nil || at.After(r.RecordedAt) || observedEffect(ctx, db.Pool, id, r.Build) != nil {
		return BuiltRecord{}, ErrExecution
	}
	r.Publish, at, e = readBuildObservation(ctx, db.Pool, id, "publish")
	if e != nil || r.Publish.Validate(r.Command, r.Plan, slot, at) != nil || at.After(r.RecordedAt) || observedEffect(ctx, db.Pool, id, r.Publish) != nil || !orderedTerminations(r.Build, r.Publish) || r.Result.Validate(r.Command, r.Plan, r.Build, r.Publish) != nil {
		return BuiltRecord{}, ErrExecution
	}
	return r, nil
}

// CanonicalResultBytes is useful for hashing evidence without logging it.
func (r BuiltRecord) Digest() string { raw, _ := json.Marshal(r); return authority.Digest(raw) }
