package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

// An uncertain publication effect (begun, neither acknowledged nor known
// unsent) quarantines all publication. Reconciliation may resolve it only from
// a read-only forge observation, and only once the original attempt can no
// longer be in flight: its claim (at most 120 s) has ended, and a further
// settle window has passed for a request the forge accepted but had not yet
// applied. The window is fixed here, not supplied by the caller.
const publicationSettle = `interval '120 seconds'`

// PublicationReconciliation is a read-only snapshot of one uncertain effect
// and the journaled facts its planned write is re-derived from. It grants no
// write: the forge is never written during reconciliation.
type PublicationReconciliation struct {
	RequestID, Repository, PayloadDigest, PlanDigest string
	Payload                                          []byte
	CommitTime                                       time.Time
	Kind, EffectToken, RequestDigest                 string
	// Completed are the acknowledged predecessors, in journal order. The
	// journal admits an effect only after every predecessor completed, so
	// at most one effect per request can be uncertain.
	Completed []PublicationEvidence
	// Consistent is false when the journaled predecessors cannot be read or
	// are not exactly the acknowledged effects before Kind. Such an effect is
	// still listed, so it is reported (and stays quarantined) without hiding
	// the others behind an error.
	Consistent bool
}

var publicationKinds = []string{"tree", "commit", "branch", "pull-request"}

// PublicationReconciliations lists up to limit uncertain effects whose
// original attempt has settled, oldest first.
func (db *DB) PublicationReconciliations(ctx context.Context, limit int) ([]PublicationReconciliation, error) {
	if limit < 1 || limit > 16 {
		return nil, ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var result []PublicationReconciliation
	e := db.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "release-manager") != nil {
			return ErrExecution
		}
		rows, e := tx.QueryContext(ctx, `SELECT r.id,r.repository,r.payload_digest,r.payload,r.plan_digest,r.commit_time,e.kind,e.token::text,e.request_digest
 FROM publication_effects e JOIN publication_requests r ON r.id=e.request_id
 WHERE r.state='failed' AND e.completed_at IS NULL AND e.not_sent_at IS NULL AND r.claim_until+`+publicationSettle+`<clock_timestamp()
 ORDER BY e.created_at,r.id LIMIT $1`, limit)
		if e != nil {
			return ErrExecution
		}
		for rows.Next() {
			var v PublicationReconciliation
			if rows.Scan(&v.RequestID, &v.Repository, &v.PayloadDigest, &v.Payload, &v.PlanDigest, &v.CommitTime, &v.Kind, &v.EffectToken, &v.RequestDigest) != nil {
				rows.Close()
				return ErrExecution
			}
			result = append(result, v)
		}
		if rows.Close() != nil || rows.Err() != nil {
			return ErrExecution
		}
		for i := range result {
			// readPublicationEffects is a plain SELECT: a rejected row does not
			// abort this transaction for the rows after it.
			completed, e := readPublicationEffects(ctx, tx, result[i].RequestID)
			if e == nil && publicationPredecessors(result[i].Kind, completed) {
				result[i].Completed, result[i].Consistent = completed, true
			}
		}
		return nil
	})
	if e != nil {
		return nil, ErrExecution
	}
	return result, nil
}

func publicationPredecessors(kind string, completed []PublicationEvidence) bool {
	for i, k := range publicationKinds {
		if k == kind {
			if len(completed) != i {
				return false
			}
			for j := range completed {
				if completed[j].Kind != publicationKinds[j] {
					return false
				}
			}
			return true
		}
	}
	return false
}

// ResolvePublicationEffect records an observed outcome for one uncertain
// effect: a receipt (the effect exists exactly as planned) or, with a nil
// receipt, a known no-send (the forge proves it absent). It re-checks, under
// the publication claim lock, that no publication is claimed, the request is
// still failed and settled, and the effect is the same uncertain intent with
// the same request digest. A resolved effect can never change again.
func (db *DB) ResolvePublicationEffect(ctx context.Context, r PublicationReconciliation, receipt *PublicationReceipt) error {
	if !publicationIDPattern.MatchString(r.RequestID) || !lifecycleUUID.MatchString(r.EffectToken) || !modelDigest.MatchString(r.RequestDigest) || !modelDigest.MatchString(r.PlanDigest) {
		return ErrExecution
	}
	var raw []byte
	if receipt != nil {
		if receipt.Kind != r.Kind || !receipt.Observed || !validPublicationReceipt(*receipt) {
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
		// The same lock ClaimPublication takes: no claim can start meanwhile.
		if _, e := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(73421025)`); e != nil {
			return ErrExecution
		}
		var idle bool
		var payload []byte
		if tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM publication_requests WHERE state='claimed'),r.payload FROM publication_effects e JOIN publication_requests r ON r.id=e.request_id
 WHERE e.request_id=$1 AND e.kind=$2 AND e.token=$3::uuid AND e.request_digest=$4 AND e.completed_at IS NULL AND e.not_sent_at IS NULL
 AND r.state='failed' AND r.plan_digest=$5 AND r.commit_time=$6 AND r.claim_until+`+publicationSettle+`<clock_timestamp()`, r.RequestID, r.Kind, r.EffectToken, r.RequestDigest, r.PlanDigest, r.CommitTime).Scan(&idle, &payload) != nil || !idle {
			return ErrExecution
		}
		completed, e := readPublicationEffects(ctx, tx, r.RequestID)
		if e != nil || !publicationPredecessors(r.Kind, completed) {
			return ErrExecution
		}
		var result sql.Result
		if receipt == nil {
			result, e = tx.ExecContext(ctx, `UPDATE publication_effects SET not_sent_at=clock_timestamp() WHERE request_id=$1 AND kind=$2 AND token=$3::uuid AND request_digest=$4 AND completed_at IS NULL AND not_sent_at IS NULL`, r.RequestID, r.Kind, r.EffectToken, r.RequestDigest)
		} else {
			// A branch or pull request points at the acknowledged commit, and a
			// pull request's head is the acknowledged branch.
			// Both name the only branch the signed command may publish to.
			branch := plannedPublicationBranch(payload)
			switch r.Kind {
			case "branch":
				if receipt.SHA != completed[1].Receipt.SHA || branch == "" || receipt.Branch != branch {
					return ErrExecution
				}
			case "pull-request":
				if receipt.SHA != completed[1].Receipt.SHA || receipt.SHA != completed[2].Receipt.SHA || branch == "" || receipt.Branch != branch || completed[2].Receipt.Branch != branch {
					return ErrExecution
				}
			}
			result, e = tx.ExecContext(ctx, `UPDATE publication_effects SET receipt=$5,receipt_digest=$6,completed_at=clock_timestamp() WHERE request_id=$1 AND kind=$2 AND token=$3::uuid AND request_digest=$4 AND completed_at IS NULL AND not_sent_at IS NULL`, r.RequestID, r.Kind, r.EffectToken, r.RequestDigest, raw, authority.Digest(raw))
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

var publicationCandidate = regexp.MustCompile(`^execution-[a-f0-9]{64}$`)

// plannedPublicationBranch mirrors publication.BranchName for the journaled
// signed command (the store cannot import the publication package).
func plannedPublicationBranch(payload []byte) string {
	var c struct {
		IssueNumber int    `json:"issueNumber"`
		CandidateID string `json:"candidateId"`
	}
	if json.Unmarshal(payload, &c) != nil || c.IssueNumber < 1 || c.IssueNumber > 2147483647 || !publicationCandidate.MatchString(c.CandidateID) {
		return ""
	}
	return fmt.Sprintf("ai-sdlc/issue-%d-%s", c.IssueNumber, c.CandidateID[len("execution-"):len("execution-")+32])
}
