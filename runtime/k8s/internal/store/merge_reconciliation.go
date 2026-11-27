package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/merging"
)

// An uncertain merge effect (begun, neither acknowledged nor known unsent)
// quarantines every later merge. Reconciliation may resolve it only from a
// read-only forge observation, and only once the original attempt can no
// longer be in flight: its claim (at most 90 s, further capped by the 30 s gate
// lifetime) has ended, so the transport, bound to the claim deadline, can no
// longer send; and a further settle window has passed for a request GitHub
// accepted but had not yet applied. The window is fixed here, not supplied by
// the caller, and equals the publication settle window.
const mergeSettle = `interval '120 seconds'`

// MergeReconciliation is a read-only snapshot of one uncertain merge effect and
// the journaled signed command its planned request is re-derived from. It
// grants no write: the forge is never written during reconciliation.
type MergeReconciliation struct {
	RequestID, PayloadDigest   string
	Payload                    []byte
	EffectToken, RequestDigest string
}

// MergeReconciliations lists up to limit uncertain merge effects whose original
// attempt has settled, oldest first.
func (db *DB) MergeReconciliations(ctx context.Context, limit int) ([]MergeReconciliation, error) {
	if limit < 1 || limit > 16 {
		return nil, ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "release-manager") != nil {
		return nil, ErrExecution
	}
	rows, e := db.Pool.QueryContext(ctx, `SELECT r.id,r.payload_digest,r.payload,e.token::text,e.request_digest
 FROM merge_effects e JOIN merge_requests r ON r.id=e.request_id
 WHERE r.state='failed' AND e.completed_at IS NULL AND e.not_sent_at IS NULL AND r.claim_until IS NOT NULL AND r.claim_until+`+mergeSettle+`<clock_timestamp()
 ORDER BY e.created_at,r.id LIMIT $1`, limit)
	if e != nil {
		return nil, ErrExecution
	}
	defer rows.Close()
	var result []MergeReconciliation
	for rows.Next() {
		var v MergeReconciliation
		if rows.Scan(&v.RequestID, &v.PayloadDigest, &v.Payload, &v.EffectToken, &v.RequestDigest) != nil {
			return nil, ErrExecution
		}
		result = append(result, v)
	}
	if rows.Err() != nil {
		return nil, ErrExecution
	}
	return result, nil
}

// ResolveMergeEffect records an observed outcome for one uncertain merge
// effect: a receipt in exactly the format the live acknowledgement stores (the
// forge shows the planned merge), or, with a nil receipt, a known no-send (the
// forge shows the pull request still open at the planned head over an
// unchanged base). Under the merge claim lock it re-checks that no merge is
// claimed, the request is still failed and settled with the same signed
// payload, and the effect is the same uncertain intent with the same request
// digest. A receipt must name the command's pull request, head, base and tree.
// The request keeps its failed state: no merged record is ever created here.
// A resolved effect can never change again.
func (db *DB) ResolveMergeEffect(ctx context.Context, r MergeReconciliation, receipt *MergeReceipt) error {
	if !mergeIDPattern.MatchString(r.RequestID) || !lifecycleUUID.MatchString(r.EffectToken) || !modelDigest.MatchString(r.RequestDigest) || !modelDigest.MatchString(r.PayloadDigest) {
		return ErrExecution
	}
	var raw []byte
	if receipt != nil {
		if !validMergeReceipt(*receipt) {
			return ErrExecution
		}
		raw, _ = json.Marshal(receipt)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "release-manager") != nil {
			return ErrExecution
		}
		// The same lock ClaimMerge takes: no claim can start meanwhile.
		if _, e := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(73421026)`); e != nil {
			return ErrExecution
		}
		var idle bool
		var payload []byte
		if tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM merge_requests WHERE state='claimed'),r.payload FROM merge_effects e JOIN merge_requests r ON r.id=e.request_id
 WHERE e.request_id=$1 AND e.token=$2::uuid AND e.request_digest=$3 AND e.completed_at IS NULL AND e.not_sent_at IS NULL
 AND r.state='failed' AND r.payload_digest=$4 AND r.claim_until IS NOT NULL AND r.claim_until+`+mergeSettle+`<clock_timestamp()`, r.RequestID, r.EffectToken, r.RequestDigest, r.PayloadDigest).Scan(&idle, &payload) != nil || !idle {
			return ErrExecution
		}
		var c merging.Command
		if authority.Digest(payload) != r.PayloadDigest || authority.DecodeStrict(payload, &c) != nil || c.ID() != r.RequestID {
			return ErrExecution
		}
		var result sql.Result
		var e error
		if receipt == nil {
			result, e = tx.ExecContext(ctx, `UPDATE merge_effects SET not_sent_at=clock_timestamp() WHERE request_id=$1 AND token=$2::uuid AND request_digest=$3 AND receipt IS NULL AND completed_at IS NULL AND not_sent_at IS NULL`, r.RequestID, r.EffectToken, r.RequestDigest)
		} else {
			if c.HeadSHA != receipt.HeadSHA || c.BaseSHA != receipt.BaseSHA || c.TreeSHA != receipt.TreeSHA || c.PullRequest != receipt.PullRequest {
				return ErrExecution
			}
			result, e = tx.ExecContext(ctx, `UPDATE merge_effects SET receipt=$4,receipt_digest=$5,completed_at=clock_timestamp() WHERE request_id=$1 AND token=$2::uuid AND request_digest=$3 AND receipt IS NULL AND completed_at IS NULL AND not_sent_at IS NULL`, r.RequestID, r.EffectToken, r.RequestDigest, raw, authority.Digest(raw))
		}
		if e != nil {
			return ErrExecution
		}
		if n, e := result.RowsAffected(); e != nil || n != 1 {
			return ErrExecution
		}
		return nil
	})
}
