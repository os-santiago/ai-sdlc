package store

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/promotionrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"time"
)

// PromotionRecord is an immutable planning decision, not deployment authority.
type PromotionRecord = promotionrecord.Record

func (db *DB) ReadPromotionRecord(ctx context.Context, id string) (PromotionRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return readPromotionRecord(ctx, db.Pool, id, "release-manager")
}
func readPromotionRecord(ctx context.Context, q Querier, id, role string) (PromotionRecord, error) {
	var r PromotionRecord
	if !promotionID.MatchString(id) || (role != "release-manager" && role != "deployer") || AssertComponent(ctx, q, role) != nil {
		return r, ErrExecution
	}
	var raw, signed, config, plan []byte
	var payloadDigest, planDigest string
	var expires time.Time
	if q.QueryRowContext(ctx, `SELECT q.payload,q.receipt,q.configuration,q.payload_digest,q.expires_at,p.plan,p.digest,p.recorded_at FROM promotion_records p JOIN promotion_requests q ON q.id=p.request_id WHERE q.id=$1 AND q.state='planned' AND p.recorded_at>=q.created_at AND p.recorded_at<q.expires_at AND p.recorded_at<=clock_timestamp()`, id).Scan(&raw, &signed, &config, &payloadDigest, &expires, &plan, &planDigest, &r.RecordedAt) != nil || authority.Digest(raw) != payloadDigest || authority.Digest(plan) != planDigest {
		return PromotionRecord{}, ErrExecution
	}
	var receipt authority.Receipt
	if authority.DecodeStrict(signed, &receipt) != nil || authority.DecodeStrict(config, &r.Configuration) != nil || authority.DecodeStrict(plan, &r.Plan) != nil {
		return PromotionRecord{}, ErrExecution
	}
	var e error
	r.Command, e = releasing.VerifyCommand(r.Configuration, receipt, raw, r.RecordedAt)
	if e != nil || r.Command.ID() != id || !expires.Equal(time.UnixMilli(r.Command.ExpiresAt)) || r.RecordedAt.Before(time.UnixMilli(r.Command.IssuedAt)) || r.Plan.CompiledAt > r.RecordedAt.UnixMilli() {
		return PromotionRecord{}, ErrExecution
	}
	p, e := readProvenanceRecord(ctx, q, r.Command.ProvenanceID, role)
	if e != nil {
		return PromotionRecord{}, ErrExecution
	}
	b, e := readBuiltRecord(ctx, q, p.Command.BuildID, role)
	if e != nil || r.Command.BindRecords(r.Configuration, p, b, r.RecordedAt) != nil || r.Plan.Validate(r.Configuration, r.Command, p, b) != nil {
		return PromotionRecord{}, ErrExecution
	}
	return r, nil
}
