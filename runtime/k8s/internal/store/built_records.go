package store

import (
	"context"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
)

// BuiltRecord is historical evidence, not verified provenance or release authority.
type BuiltRecord = buildrecord.Record

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
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return readBuiltRecord(ctx, db.Pool, id, role)
}
func readBuiltRecord(ctx context.Context, q Querier, id, role string) (BuiltRecord, error) {
	var r BuiltRecord
	if !buildIDPattern.MatchString(id) || (role != "verifier" && role != "build-controller" && role != "release-manager" && role != "deployer" && role != "orchestrator") || AssertComponent(ctx, q, role) != nil {
		return r, ErrExecution
	}
	var payload, receipt, inner, outer, raw, slotRaw []byte
	var digest, slotDigest string
	var expires time.Time
	if q.QueryRowContext(ctx, `SELECT q.payload,q.receipt,q.inner_policy,q.outer_policy,f.result,f.digest,f.recorded_at,s.slot,s.slot_digest,q.expires_at FROM built_records f JOIN build_requests q ON q.id=f.request_id JOIN build_slots s ON s.request_id=q.id WHERE q.id=$1 AND q.state='built' AND f.recorded_at>=q.created_at AND f.recorded_at<q.expires_at AND f.recorded_at<=clock_timestamp()`, id).Scan(&payload, &receipt, &inner, &outer, &raw, &digest, &r.RecordedAt, &slotRaw, &slotDigest, &expires) != nil {
		return BuiltRecord{}, ErrExecution
	}
	var e error
	r.Command, e = archivedBuild(payload, receipt, inner, outer)
	var slot building.Slot
	if e != nil || !expires.Equal(time.UnixMilli(r.Command.ExpiresAt)) || r.RecordedAt.Before(time.UnixMilli(r.Command.IssuedAt)) || !r.RecordedAt.Before(time.UnixMilli(r.Command.ExpiresAt)) || r.Command.ID() != id || authority.DecodeStrict(raw, &r.Result) != nil || authority.Digest(raw) != digest || authority.DecodeStrict(slotRaw, &slot) != nil || slot.Validate() != nil || slot.Digest() != slotDigest {
		return BuiltRecord{}, ErrExecution
	}
	r.Plan, e = readBuildPlan(ctx, q, id)
	if e != nil || r.Plan.Validate(r.Command, slot) != nil {
		return BuiltRecord{}, ErrExecution
	}
	var contextRaw []byte
	var contextDigest string
	var contextAt time.Time
	if q.QueryRowContext(ctx, `SELECT c.context,c.digest,c.recorded_at FROM build_contexts c JOIN build_effects e ON e.request_id=c.request_id AND e.kind='build' JOIN build_plans p ON p.request_id=c.request_id WHERE c.request_id=$1 AND c.recorded_at<=e.created_at AND c.recorded_at<=p.created_at`, id).Scan(&contextRaw, &contextDigest, &contextAt) != nil || authority.Digest(contextRaw) != contextDigest || contextAt.Before(time.UnixMilli(r.Command.IssuedAt)) || contextAt.After(r.RecordedAt) {
		return BuiltRecord{}, ErrExecution
	}
	r.Context, e = buildworkload.DecodeRetained(contextRaw)
	if e != nil || r.Context.Slot != slot || r.Context.ValidateHistorical(r.Command, r.Plan) != nil {
		return BuiltRecord{}, ErrExecution
	}
	var at time.Time
	r.Build, at, e = readBuildObservation(ctx, q, id, "build")
	if e != nil || r.Build.Validate(r.Command, r.Plan, slot, at) != nil || at.After(r.RecordedAt) || observedEffect(ctx, q, id, r.Build) != nil {
		return BuiltRecord{}, ErrExecution
	}
	r.Publish, at, e = readBuildObservation(ctx, q, id, "publish")
	if e != nil || r.Publish.Validate(r.Command, r.Plan, slot, at) != nil || at.After(r.RecordedAt) || observedEffect(ctx, q, id, r.Publish) != nil || !orderedTerminations(r.Build, r.Publish) || r.Result.Validate(r.Command, r.Plan, r.Build, r.Publish) != nil {
		return BuiltRecord{}, ErrExecution
	}
	return r, nil
}
