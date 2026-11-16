package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/merging"
)

// MergedRecord is a historical fact, never deployment or renewed merge authority.
type MergedRecord struct {
	MergeID, PayloadDigest, ProofDigest, EffectDigest string
	Payload, Receipt, Proof, Effect, Gate, Policy     []byte
	MergedAt, AuthorityExpiresAt                      time.Time
}

func (db *DB) ReadMergedRecord(ctx context.Context, id string) (MergedRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return readMergedRecord(ctx, db.Pool, id, "release-manager")
}

func readMergedRecord(ctx context.Context, q Querier, id, role string) (MergedRecord, error) {
	var r MergedRecord
	if (role != "release-manager" && role != "build-controller" && role != "verifier") || !mergeIDPattern.MatchString(id) || AssertComponent(ctx, q, role) != nil {
		return r, ErrExecution
	}
	e := q.QueryRowContext(ctx, `SELECT f.merge_id,f.payload_digest,f.payload,f.receipt,f.gate,f.policy,f.proof_digest,f.proof,f.effect_digest,f.effect,f.merged_at,f.authority_expires_at FROM merged_records f JOIN merge_requests r ON r.id=f.merge_id WHERE f.merge_id=$1 AND r.state='merged' AND r.payload_digest=f.payload_digest AND r.payload=f.payload AND r.receipt=f.receipt AND r.gate=f.gate AND r.policy=f.policy AND r.proof=f.proof AND r.expires_at=f.authority_expires_at AND f.merged_at>=r.created_at AND f.merged_at<=clock_timestamp()`, id).Scan(&r.MergeID, &r.PayloadDigest, &r.Payload, &r.Receipt, &r.Gate, &r.Policy, &r.ProofDigest, &r.Proof, &r.EffectDigest, &r.Effect, &r.MergedAt, &r.AuthorityExpiresAt)
	if e != nil {
		return MergedRecord{}, observationError(e)
	}
	var archived authority.Policy
	if authority.DecodeStrict(r.Policy, &archived) != nil || verifyMergeAuthority(archived, r.Payload, r.Receipt, r.Gate, r.MergedAt) != nil {
		return MergedRecord{}, ErrExecution
	}
	var c merging.Command
	var p MergeProof
	var effect MergeEvidence
	if authority.Digest(r.Payload) != r.PayloadDigest || authority.Digest(r.Proof) != r.ProofDigest || authority.Digest(r.Effect) != r.EffectDigest || !r.MergedAt.Before(r.AuthorityExpiresAt) || authority.DecodeStrict(r.Payload, &c) != nil || authority.DecodeStrict(r.Proof, &p) != nil || authority.DecodeStrict(r.Effect, &effect) != nil {
		return MergedRecord{}, ErrExecution
	}
	raw, _ := json.Marshal(effect.Receipt)
	if c.Version != merging.Version || c.ID() != id || p.Version != "ai-sdlc.merge-proof/v1" || p.MergeID != id || p.PublicationID != c.PublicationID || p.PublicationRecordSHA256 != c.PublicationRecordSHA256 || p.GateSHA256 != c.GateSHA256 || p.EffectSHA256 != r.EffectDigest || p.DeploymentAuthorized || p.Receipt != effect.Receipt || !validMergeReceipt(p.Receipt) || !modelDigest.MatchString(effect.RequestDigest) || authority.Digest(raw) != effect.ReceiptDigest || p.Receipt.HeadSHA != c.HeadSHA || p.Receipt.BaseSHA != c.BaseSHA || p.Receipt.TreeSHA != c.TreeSHA || p.Receipt.PullRequest != c.PullRequest || !time.UnixMilli(c.ExpiresAt).Equal(r.AuthorityExpiresAt) {
		return MergedRecord{}, ErrExecution
	}
	return r, nil
}
