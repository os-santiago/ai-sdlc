package store

import (
	"context"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"time"
)

type PublicationProof struct {
	Version              string `json:"version"`
	PublicationID        string `json:"publicationID"`
	PlanSHA256           string `json:"planSHA256"`
	EffectsSHA256        string `json:"effectsSHA256"`
	CommitSHA            string `json:"commitSHA"`
	Branch               string `json:"branch"`
	PullRequest          int    `json:"pullRequest"`
	MergeAuthorized      bool   `json:"mergeAuthorized"`
	DeploymentAuthorized bool   `json:"deploymentAuthorized"`
}

// PublishedRecord is immutable historical evidence. Reading it confers no
// authority to merge, publish, renew a claim, release or deploy anything.
// A later stage must independently acquire current bounded authority.
type PublishedRecord struct {
	PublicationID, PayloadDigest, ProofDigest, EffectsDigest string
	Payload, Receipt, Proof, Effects                         []byte
	PublishedAt, AuthorityExpiresAt                          time.Time
}

// ReadPublishedRecord does not use an expired generation/publication claim.
// It requires the original atomic terminal record; legacy/failed/pending rows
// and records inconsistent with their signed publication journal fail closed.
func (db *DB) ReadPublishedRecord(ctx context.Context, id string) (PublishedRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return readPublishedRecord(ctx, db.Pool, id)
}

func readPublishedRecord(ctx context.Context, q Querier, id string) (PublishedRecord, error) {
	return readPublishedRecordAs(ctx, q, id, "release-manager")
}
func readPublishedRecordAs(ctx context.Context, q Querier, id, role string) (PublishedRecord, error) {
	var r PublishedRecord
	var planDigest string
	if (role != "release-manager" && role != "build-controller" && role != "verifier" && role != "deployer" && role != "orchestrator") || !publicationIDPattern.MatchString(id) || AssertComponent(ctx, q, role) != nil {
		return r, ErrExecution
	}
	err := q.QueryRowContext(ctx, `SELECT f.publication_id,f.payload_digest,f.payload,f.receipt,f.proof_digest,f.proof,f.effects_digest,f.effects,f.published_at,f.authority_expires_at,r.plan_digest FROM published_records f JOIN publication_requests r ON r.id=f.publication_id WHERE f.publication_id=$1 AND r.state='published' AND r.payload_digest=f.payload_digest AND r.payload=f.payload AND r.receipt=f.receipt AND r.proof=f.proof AND f.authority_expires_at=r.expires_at AND f.published_at<=clock_timestamp() AND f.published_at>=r.commit_time`, id).Scan(&r.PublicationID, &r.PayloadDigest, &r.Payload, &r.Receipt, &r.ProofDigest, &r.Proof, &r.EffectsDigest, &r.Effects, &r.PublishedAt, &r.AuthorityExpiresAt, &planDigest)
	if err != nil {
		return PublishedRecord{}, observationError(err)
	}
	var p PublicationProof
	var effects []PublicationEvidence
	if authority.Digest(r.Payload) != r.PayloadDigest || authority.Digest(r.Proof) != r.ProofDigest || authority.Digest(r.Effects) != r.EffectsDigest || !r.PublishedAt.Before(r.AuthorityExpiresAt) || authority.DecodeStrict(r.Proof, &p) != nil || authority.DecodeStrict(r.Effects, &effects) != nil || len(effects) != 4 || p.Version != "ai-sdlc.publication-proof/v1" || p.PublicationID != id || p.EffectsSHA256 != r.EffectsDigest || !modelDigest.MatchString(p.PlanSHA256) || p.PlanSHA256 != planDigest || p.MergeAuthorized || p.DeploymentAuthorized {
		return PublishedRecord{}, ErrExecution
	}
	for i, kind := range []string{"tree", "commit", "branch", "pull-request"} {
		effect := effects[i]
		raw, _ := json.Marshal(effect.Receipt)
		if effect.Kind != kind || effect.Receipt.Kind != kind || !validPublicationReceipt(effect.Receipt) || !modelDigest.MatchString(effect.RequestDigest) || authority.Digest(raw) != effect.ReceiptDigest {
			return PublishedRecord{}, ErrExecution
		}
	}
	commit, branch, pr := effects[1].Receipt, effects[2].Receipt, effects[3].Receipt
	if commit.SHA != p.CommitSHA || branch.SHA != p.CommitSHA || pr.SHA != p.CommitSHA || branch.Branch != p.Branch || pr.Branch != p.Branch || pr.PullRequest != p.PullRequest {
		return PublishedRecord{}, ErrExecution
	}
	return r, nil
}

// Digest binds immutable evidence bytes and the database publication time. It
// does not validate a caller-constructed struct or grant any authority.
func (r PublishedRecord) Digest() string {
	r.PublishedAt = r.PublishedAt.UTC()
	r.AuthorityExpiresAt = r.AuthorityExpiresAt.UTC()
	raw, _ := json.Marshal(r)
	return authority.Digest(raw)
}
