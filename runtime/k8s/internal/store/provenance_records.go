package store

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenancerecord"
	"time"
)

// ProvenanceRecord preserves the historical wire encoding. Authenticity is established only by this role-checked reader.
type ProvenanceRecord = provenancerecord.Record

func (db *DB) ReadProvenanceRecord(ctx context.Context, id string) (ProvenanceRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return readProvenanceRecord(ctx, db.Pool, id, "verifier")
}
func readProvenanceRecord(ctx context.Context, q Querier, id, role string) (ProvenanceRecord, error) {
	var r ProvenanceRecord
	if !provenanceID.MatchString(id) || (role != "verifier" && role != "release-manager" && role != "deployer" && role != "orchestrator") || AssertComponent(ctx, q, role) != nil {
		return r, ErrExecution
	}
	var raw, signed, config, proof []byte
	var digest, payloadDigest string
	var expires time.Time
	if q.QueryRowContext(ctx, `SELECT q.payload,q.receipt,q.configuration,q.payload_digest,q.expires_at,p.proof,p.digest,p.recorded_at FROM provenance_records p JOIN provenance_requests q ON q.id=p.request_id WHERE q.id=$1 AND q.state='verified' AND p.recorded_at>=q.created_at AND p.recorded_at<q.expires_at AND p.recorded_at<=clock_timestamp()`, id).Scan(&raw, &signed, &config, &payloadDigest, &expires, &proof, &digest, &r.RecordedAt) != nil || authority.Digest(raw) != payloadDigest || authority.Digest(proof) != digest {
		return ProvenanceRecord{}, ErrExecution
	}
	var receipt authority.Receipt
	if authority.DecodeStrict(signed, &receipt) != nil || authority.DecodeStrict(config, &r.Configuration) != nil || authority.DecodeStrict(proof, &r.Proof) != nil {
		return ProvenanceRecord{}, ErrExecution
	}
	var e error
	r.Command, e = provenance.VerifyCommand(r.Configuration, receipt, raw, r.RecordedAt)
	if e != nil || r.Command.ID() != id || !expires.Equal(time.UnixMilli(r.Command.ExpiresAt)) || r.RecordedAt.Before(time.UnixMilli(r.Command.IssuedAt)) {
		return ProvenanceRecord{}, ErrExecution
	}
	built, e := readBuiltRecord(ctx, q, r.Command.BuildID, role)
	if e != nil || r.Command.BindProof(r.Configuration, built, r.Proof, r.RecordedAt) != nil {
		return ProvenanceRecord{}, ErrExecution
	}
	return r, nil
}
