package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/merging"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	"time"
)

type MergeEffect struct {
	Claim                MergeClaim
	Token, RequestDigest string
}
type MergeReceipt struct {
	Version     string `json:"version"`
	CommitSHA   string `json:"commitSHA"`
	HeadSHA     string `json:"headSHA"`
	BaseSHA     string `json:"baseSHA"`
	TreeSHA     string `json:"treeSHA"`
	PullRequest int    `json:"pullRequest"`
	// Observed marks a receipt recorded by merge reconciliation from a
	// read-only forge observation. It is omitted for every live write, so
	// live receipts and their digests are unchanged; no live boundary accepts
	// an observed receipt.
	Observed bool `json:"observed,omitempty"`
}
type MergeEvidence struct {
	RequestDigest, ReceiptDigest string
	Receipt                      MergeReceipt
}
type MergeProof struct {
	Version                 string       `json:"version"`
	MergeID                 string       `json:"mergeID"`
	PublicationID           string       `json:"publicationID"`
	PublicationRecordSHA256 string       `json:"publicationRecordSHA256"`
	GateSHA256              string       `json:"gateSHA256"`
	EffectSHA256            string       `json:"effectSHA256"`
	Receipt                 MergeReceipt `json:"receipt"`
	DeploymentAuthorized    bool         `json:"deploymentAuthorized"`
}

func validMergeReceipt(r MergeReceipt) bool {
	if r.Version != "ai-sdlc.merge-receipt/v1" || r.PullRequest < 1 || r.PullRequest > 2147483647 || r.CommitSHA == r.HeadSHA || r.CommitSHA == r.BaseSHA || r.HeadSHA == r.BaseSHA {
		return false
	}
	for _, sha := range []string{r.CommitSHA, r.HeadSHA, r.BaseSHA, r.TreeSHA} {
		if !workspacebundle.ValidSourceSHA(sha) {
			return false
		}
	}
	return true
}

// liveMergeReceipt is a receipt the transport returned under a live claim:
// the only kind an acknowledgement, a merge proof or a merged record accepts.
func liveMergeReceipt(r MergeReceipt) bool { return validMergeReceipt(r) && !r.Observed }
func (j *MergeJournal) BeginMergeEffect(ctx context.Context, c MergeClaim, digest string) (MergeEffect, error) {
	if !modelDigest.MatchString(digest) {
		return MergeEffect{}, ErrExecution
	}
	effect := MergeEffect{c, NewID(), digest}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if j.lockMerge(ctx, tx, c) != nil {
			return ErrExecution
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO merge_effects(request_id,token,request_digest) VALUES($1,$2,$3)`, c.ID, effect.Token, digest); e != nil {
			return ErrExecution
		}
		return j.checkMergeTime(ctx, tx, c)
	})
	if e != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = j.StopMerge(cleanup, c, "uncertain")
		return MergeEffect{}, ErrExecution
	}
	return effect, nil
}
func (j *MergeJournal) AcknowledgeMergeEffect(ctx context.Context, effect MergeEffect, r MergeReceipt) error {
	if !liveMergeReceipt(r) || !lifecycleUUID.MatchString(effect.Token) || !modelDigest.MatchString(effect.RequestDigest) {
		return ErrExecution
	}
	raw, _ := json.Marshal(r)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if j.lockMerge(ctx, tx, effect.Claim) != nil {
			return ErrExecution
		}
		var payload []byte
		var c merging.Command
		if tx.QueryRowContext(ctx, `SELECT payload FROM merge_requests WHERE id=$1`, effect.Claim.ID).Scan(&payload) != nil || authority.DecodeStrict(payload, &c) != nil || c.ID() != effect.Claim.ID || c.HeadSHA != r.HeadSHA || c.BaseSHA != r.BaseSHA || c.TreeSHA != r.TreeSHA || c.PullRequest != r.PullRequest {
			return ErrExecution
		}
		result, e := tx.ExecContext(ctx, `UPDATE merge_effects SET receipt=$4,receipt_digest=$5,completed_at=clock_timestamp() WHERE request_id=$1 AND token=$2 AND request_digest=$3 AND receipt IS NULL AND completed_at IS NULL AND not_sent_at IS NULL`, effect.Claim.ID, effect.Token, effect.RequestDigest, raw, authority.Digest(raw))
		if e != nil {
			return ErrExecution
		}
		n, e := result.RowsAffected()
		if e != nil || n != 1 {
			return ErrExecution
		}
		return j.checkMergeTime(ctx, tx, effect.Claim)
	})
}

// Only a trusted pre-send callback can attest that no transport was attempted.
// This seals an intention; it cannot authorize replay or clear an unknown send.
func (j *MergeJournal) SealMergeNotSent(ctx context.Context, effect MergeEffect) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, j.DB.Pool, "release-manager") != nil {
		return ErrExecution
	}
	result, e := j.DB.Pool.ExecContext(ctx, `UPDATE merge_effects e SET not_sent_at=clock_timestamp() FROM merge_requests r WHERE e.request_id=r.id AND r.id=$1 AND r.claim_token=$2 AND e.token=$3 AND e.request_digest=$4 AND e.completed_at IS NULL AND e.not_sent_at IS NULL`, effect.Claim.ID, effect.Claim.Token, effect.Token, effect.RequestDigest)
	if e != nil {
		return ErrExecution
	}
	n, e := result.RowsAffected()
	if e != nil || n != 1 {
		return ErrExecution
	}
	return nil
}
func readMergeEvidence(ctx context.Context, tx *sql.Tx, id string) (MergeEvidence, error) {
	var e MergeEvidence
	var raw []byte
	if tx.QueryRowContext(ctx, `SELECT request_digest,receipt_digest,receipt FROM merge_effects WHERE request_id=$1 AND completed_at IS NOT NULL AND not_sent_at IS NULL`, id).Scan(&e.RequestDigest, &e.ReceiptDigest, &raw) != nil || authority.Digest(raw) != e.ReceiptDigest || authority.DecodeStrict(raw, &e.Receipt) != nil || !liveMergeReceipt(e.Receipt) {
		return MergeEvidence{}, ErrExecution
	}
	return e, nil
}
func (j *MergeJournal) ReadMergeEvidence(ctx context.Context, c MergeClaim) (MergeEvidence, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var result MergeEvidence
	e := j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if j.lockMerge(ctx, tx, c) != nil {
			return ErrExecution
		}
		var e error
		result, e = readMergeEvidence(ctx, tx, c.ID)
		return e
	})
	if e != nil {
		return MergeEvidence{}, ErrExecution
	}
	return result, nil
}
func (j *MergeJournal) FinishMerge(ctx context.Context, c MergeClaim, raw []byte) error {
	var p MergeProof
	if len(raw) == 0 || len(raw) > 16384 || authority.DecodeStrict(raw, &p) != nil || p.Version != "ai-sdlc.merge-proof/v1" || p.MergeID != c.ID || p.PublicationRecordSHA256 != c.RecordDigest || p.GateSHA256 != c.GateDigest || !publicationIDPattern.MatchString(p.PublicationID) || !modelDigest.MatchString(p.EffectSHA256) || !liveMergeReceipt(p.Receipt) || p.DeploymentAuthorized {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if j.lockMerge(ctx, tx, c) != nil {
			return ErrExecution
		}
		evidence, e := readMergeEvidence(ctx, tx, c.ID)
		if e != nil || evidence.Receipt != p.Receipt {
			return ErrExecution
		}
		effect, _ := json.Marshal(evidence)
		if authority.Digest(effect) != p.EffectSHA256 {
			return ErrExecution
		}
		result, e := tx.ExecContext(ctx, `INSERT INTO merged_records(merge_id,payload_digest,payload,receipt,gate,policy,proof_digest,proof,effect_digest,effect,authority_expires_at) SELECT id,payload_digest,payload,receipt,gate,policy,$3,$4,$5,$6,expires_at FROM merge_requests WHERE id=$1 AND publication_id=$2 AND state='claimed' AND claim_until>clock_timestamp() AND expires_at>clock_timestamp()`, c.ID, p.PublicationID, authority.Digest(raw), raw, authority.Digest(effect), effect)
		if e != nil {
			return ErrExecution
		}
		n, e := result.RowsAffected()
		if e != nil || n != 1 {
			return ErrExecution
		}
		if _, e = tx.ExecContext(ctx, `UPDATE merge_requests SET state='merged',proof=$2,merged_at=clock_timestamp() WHERE id=$1`, c.ID, raw); e != nil {
			return ErrExecution
		}
		return j.checkMergeTime(ctx, tx, c)
	})
}
