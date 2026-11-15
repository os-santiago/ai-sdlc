package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

var publicationIDPattern = regexp.MustCompile(`^publication-[a-f0-9]{64}$`)

type PublicationRequest struct {
	ID, ReviewID, VerificationID, CandidateID, Repository, PayloadDigest string
	Payload, Receipt                                                     []byte
	ExpiresAt                                                            time.Time
}
type PublicationClaim struct {
	ID, Token, PlanDigest string
	Until, CommitTime     time.Time
}

// InsertPublication is called only by the signed inbox, in the same transaction
// as its receipt. It also binds the verifier's committed handoff and passed proof.
func InsertPublication(ctx context.Context, tx *sql.Tx, r PublicationRequest, reviewDigest, behaviorDigest string) error {
	if tx == nil || AssertComponent(ctx, tx, "release-manager") != nil || !publicationIDPattern.MatchString(r.ID) || r.Repository != "os-santiago/homedir" || authority.Digest(r.Payload) != r.PayloadDigest || !modelDigest.MatchString(reviewDigest) || !modelDigest.MatchString(behaviorDigest) {
		return ErrExecution
	}
	var review, behavior []byte
	if tx.QueryRowContext(ctx, `SELECT r.proof,v.proof FROM review_runs r JOIN verification_requests v ON v.id=r.verification_id JOIN publication_handoffs h ON h.review_id=r.id WHERE r.id=$1 AND r.verification_id=$2 AND r.candidate_id=$3 AND h.command_id=$4 AND h.payload_digest=$5 AND r.state='passed' AND r.cleanup_state='complete' AND v.state='passed' AND v.cleanup_state='complete' AND r.expires_at>clock_timestamp() AND v.expires_at>clock_timestamp() AND $6<=r.expires_at AND $6<=v.expires_at`, r.ReviewID, r.VerificationID, r.CandidateID, r.ID, r.PayloadDigest, r.ExpiresAt).Scan(&review, &behavior) != nil || authority.Digest(review) != reviewDigest || authority.Digest(behavior) != behaviorDigest {
		return ErrExecution
	}
	result, e := tx.ExecContext(ctx, `INSERT INTO publication_requests(id,review_id,verification_id,candidate_id,repository,payload_digest,payload,receipt,expires_at) SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9 WHERE $9>clock_timestamp()`, r.ID, r.ReviewID, r.VerificationID, r.CandidateID, r.Repository, r.PayloadDigest, r.Payload, r.Receipt, r.ExpiresAt)
	if e != nil {
		return ErrExecution
	}
	n, e := result.RowsAffected()
	if e != nil || n != 1 {
		return ErrExecution
	}
	return nil
}
func (db *DB) ReadPublication(ctx context.Context, id string) (PublicationRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var r PublicationRequest
	if e := AssertComponent(ctx, db.Pool, "release-manager"); e != nil {
		return r, identityObservationError(e)
	}
	e := db.Pool.QueryRowContext(ctx, `SELECT id,review_id,verification_id,candidate_id,repository,payload_digest,payload,receipt,expires_at FROM publication_requests WHERE id=$1`, id).Scan(&r.ID, &r.ReviewID, &r.VerificationID, &r.CandidateID, &r.Repository, &r.PayloadDigest, &r.Payload, &r.Receipt, &r.ExpiresAt)
	if e != nil {
		return PublicationRequest{}, observationError(e)
	}
	return r, nil
}
func (db *DB) ClaimPublication(ctx context.Context, id, planDigest string) (PublicationClaim, error) {
	c := PublicationClaim{ID: id, Token: NewID(), PlanDigest: planDigest}
	if !publicationIDPattern.MatchString(id) || !modelDigest.MatchString(planDigest) {
		return PublicationClaim{}, ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := db.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "release-manager") != nil {
			return ErrExecution
		}
		if _, e := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(73421025)`); e != nil {
			return ErrExecution
		}
		var blocked bool
		if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM publication_requests WHERE state='claimed') OR EXISTS(SELECT 1 FROM publication_effects e JOIN publication_requests r ON r.id=e.request_id WHERE r.state='failed' AND e.completed_at IS NULL AND e.not_sent_at IS NULL)`).Scan(&blocked) != nil || blocked {
			return ErrExecution
		}
		if tx.QueryRowContext(ctx, `UPDATE publication_requests SET state='claimed',claim_token=$2,claim_until=LEAST(expires_at,clock_timestamp()+interval '120 seconds'),plan_digest=$3,commit_time=date_trunc('second',clock_timestamp()) WHERE id=$1 AND state='queued' AND expires_at>clock_timestamp() RETURNING claim_until,commit_time`, id, c.Token, planDigest).Scan(&c.Until, &c.CommitTime) != nil {
			return ErrExecution
		}
		return nil
	})
	if e != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = db.StopPublication(cleanup, c, "uncertain")
		return PublicationClaim{}, ErrExecution
	}
	return c, nil
}
func lockPublication(ctx context.Context, tx *sql.Tx, c PublicationClaim) error {
	if AssertComponent(ctx, tx, "release-manager") != nil || !publicationIDPattern.MatchString(c.ID) || !lifecycleUUID.MatchString(c.Token) || !modelDigest.MatchString(c.PlanDigest) {
		return ErrExecution
	}
	var ok bool
	if tx.QueryRowContext(ctx, `SELECT claim_until>clock_timestamp() AND expires_at>clock_timestamp() FROM publication_requests WHERE id=$1 AND claim_token=$2 AND plan_digest=$3 AND state='claimed' AND commit_time=$4 FOR UPDATE`, c.ID, c.Token, c.PlanDigest, c.CommitTime).Scan(&ok) != nil || !ok {
		return ErrExecution
	}
	return nil
}
func checkPublicationTime(ctx context.Context, tx *sql.Tx, c PublicationClaim) error {
	var ok bool
	if tx.QueryRowContext(ctx, `SELECT claim_until>clock_timestamp() AND expires_at>clock_timestamp() FROM publication_requests WHERE id=$1 AND claim_token=$2`, c.ID, c.Token).Scan(&ok) != nil || !ok {
		return ErrExecution
	}
	return nil
}
func (db *DB) CheckPublication(ctx context.Context, c PublicationClaim) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return db.WithTx(ctx, func(tx *sql.Tx) error { return lockPublication(ctx, tx, c) })
}
func (db *DB) StopPublication(ctx context.Context, c PublicationClaim, reason string) error {
	if reason != "expired" && reason != "revoked" && reason != "uncertain" && reason != "rejected" {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "release-manager") != nil {
		return ErrExecution
	}
	result, e := db.Pool.ExecContext(ctx, `UPDATE publication_requests SET state='failed',reason=$3 WHERE id=$1 AND claim_token=$2 AND state='claimed'`, c.ID, c.Token, reason)
	if e != nil {
		return ErrExecution
	}
	n, e := result.RowsAffected()
	if e != nil || n != 1 {
		return ErrExecution
	}
	return nil
}
func (db *DB) NextPublication(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if e := AssertComponent(ctx, db.Pool, "release-manager"); e != nil {
		return "", identityObservationError(e)
	}
	var id string
	e := db.Pool.QueryRowContext(ctx, `SELECT id FROM publication_requests WHERE state='queued' AND expires_at>clock_timestamp() ORDER BY created_at,id LIMIT 1`).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	if e != nil {
		return "", observationError(e)
	}
	return id, nil
}
func (db *DB) ExpirePublications(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "release-manager") != nil {
		return ErrExecution
	}
	_, e := db.Pool.ExecContext(ctx, `UPDATE publication_requests SET state='failed',reason='expired' WHERE state IN ('queued','claimed') AND (expires_at<=clock_timestamp() OR claim_until<=clock_timestamp())`)
	if e != nil {
		return ErrExecution
	}
	return nil
}
func (db *DB) PublicationBlocked(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "release-manager") != nil {
		return false, ErrExecution
	}
	var blocked bool
	e := db.Pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM publication_effects e JOIN publication_requests r ON r.id=e.request_id WHERE r.state='failed' AND e.completed_at IS NULL AND e.not_sent_at IS NULL)`).Scan(&blocked)
	if e != nil {
		return false, ErrExecution
	}
	return blocked, nil
}

// PublicationEffect acknowledges checked transport metadata, never API response
// bodies or generated source. Its request hash is fixed before the single POST.
type PublicationEffect struct {
	Claim                      PublicationClaim
	Kind, Token, RequestDigest string
}
type PublicationReceipt struct {
	Version     string `json:"version"`
	Kind        string `json:"kind"`
	SHA         string `json:"sha"`
	Branch      string `json:"branch,omitempty"`
	PullRequest int    `json:"pullRequest,omitempty"`
}

func validPublicationReceipt(r PublicationReceipt) bool {
	if r.Version != "ai-sdlc.publication-receipt/v1" || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(r.SHA) {
		return false
	}
	switch r.Kind {
	case "tree", "commit":
		return r.Branch == "" && r.PullRequest == 0
	case "branch":
		return regexp.MustCompile(`^ai-sdlc/issue-[1-9][0-9]{0,9}-[a-f0-9]{32}$`).MatchString(r.Branch) && r.PullRequest == 0
	case "pull-request":
		return regexp.MustCompile(`^ai-sdlc/issue-[1-9][0-9]{0,9}-[a-f0-9]{32}$`).MatchString(r.Branch) && r.PullRequest > 0 && r.PullRequest <= 2147483647
	}
	return false
}
func (db *DB) BeginPublicationEffect(ctx context.Context, c PublicationClaim, kind, digest string) (PublicationEffect, error) {
	order := map[string]int{"tree": 0, "commit": 1, "branch": 2, "pull-request": 3}
	previous, ok := order[kind]
	if !ok || !modelDigest.MatchString(digest) {
		return PublicationEffect{}, ErrExecution
	}
	effect := PublicationEffect{c, kind, NewID(), digest}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := db.WithTx(ctx, func(tx *sql.Tx) error {
		if lockPublication(ctx, tx, c) != nil {
			return ErrExecution
		}
		var total, done int
		if tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER(WHERE completed_at IS NOT NULL AND not_sent_at IS NULL) FROM publication_effects WHERE request_id=$1`, c.ID).Scan(&total, &done) != nil || total != previous || done != previous {
			return ErrExecution
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO publication_effects(request_id,kind,token,request_digest) VALUES($1,$2,$3,$4)`, c.ID, kind, effect.Token, digest); e != nil {
			return ErrExecution
		}
		return checkPublicationTime(ctx, tx, c)
	})
	if e != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = db.StopPublication(cleanup, c, "uncertain")
		return PublicationEffect{}, ErrExecution
	}
	return effect, nil
}
func (db *DB) AcknowledgePublicationEffect(ctx context.Context, effect PublicationEffect, receipt PublicationReceipt) error {
	if receipt.Kind != effect.Kind || !validPublicationReceipt(receipt) {
		return ErrExecution
	}
	raw, _ := json.Marshal(receipt)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		if lockPublication(ctx, tx, effect.Claim) != nil {
			return ErrExecution
		}
		result, e := tx.ExecContext(ctx, `UPDATE publication_effects SET receipt=$5,receipt_digest=$6,completed_at=clock_timestamp() WHERE request_id=$1 AND kind=$2 AND token=$3 AND request_digest=$4 AND completed_at IS NULL AND not_sent_at IS NULL`, effect.Claim.ID, effect.Kind, effect.Token, effect.RequestDigest, raw, authority.Digest(raw))
		if e != nil {
			return ErrExecution
		}
		n, e := result.RowsAffected()
		if e != nil || n != 1 {
			return ErrExecution
		}
		return checkPublicationTime(ctx, tx, effect.Claim)
	})
}

// SealPublicationNotSent applies only when the trusted caller knows it did not
// enter the transport at all. It never converts an observed HTTP failure into a
// known no-send outcome, nor can it change an acknowledged effect.
func (db *DB) SealPublicationNotSent(ctx context.Context, effect PublicationEffect) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "release-manager") != nil {
		return ErrExecution
	}
	result, e := db.Pool.ExecContext(ctx, `UPDATE publication_effects e SET not_sent_at=clock_timestamp() FROM publication_requests r WHERE e.request_id=$1 AND e.kind=$2 AND e.token=$3 AND e.request_digest=$4 AND e.completed_at IS NULL AND e.not_sent_at IS NULL AND r.id=e.request_id AND r.claim_token=$5`, effect.Claim.ID, effect.Kind, effect.Token, effect.RequestDigest, effect.Claim.Token)
	if e != nil {
		return ErrExecution
	}
	n, e := result.RowsAffected()
	if e != nil || n != 1 {
		return ErrExecution
	}
	return nil
}

type PublicationEvidence struct {
	Kind, RequestDigest, ReceiptDigest string
	Receipt                            PublicationReceipt
}

func readPublicationEffects(ctx context.Context, tx *sql.Tx, id string) ([]PublicationEvidence, error) {
	rows, e := tx.QueryContext(ctx, `SELECT kind,request_digest,receipt_digest,receipt FROM publication_effects WHERE request_id=$1 AND completed_at IS NOT NULL AND not_sent_at IS NULL ORDER BY CASE kind WHEN 'tree' THEN 1 WHEN 'commit' THEN 2 WHEN 'branch' THEN 3 ELSE 4 END`, id)
	if e != nil {
		return nil, ErrExecution
	}
	defer rows.Close()
	result := []PublicationEvidence{}
	for rows.Next() {
		var v PublicationEvidence
		var raw []byte
		if rows.Scan(&v.Kind, &v.RequestDigest, &v.ReceiptDigest, &raw) != nil || authority.Digest(raw) != v.ReceiptDigest || authority.DecodeStrict(raw, &v.Receipt) != nil || !validPublicationReceipt(v.Receipt) || v.Kind != v.Receipt.Kind {
			return nil, ErrExecution
		}
		result = append(result, v)
	}
	if rows.Err() != nil {
		return nil, ErrExecution
	}
	return result, nil
}
func (db *DB) ReadPublicationEffects(ctx context.Context, c PublicationClaim) ([]PublicationEvidence, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var result []PublicationEvidence
	e := db.WithTx(ctx, func(tx *sql.Tx) error {
		if lockPublication(ctx, tx, c) != nil {
			return ErrExecution
		}
		var e error
		result, e = readPublicationEffects(ctx, tx, c.ID)
		return e
	})
	if e != nil {
		return nil, ErrExecution
	}
	return result, nil
}
func (db *DB) FinishPublication(ctx context.Context, c PublicationClaim, proof []byte) error {
	var p struct {
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
	if len(proof) > 32768 || authority.DecodeStrict(proof, &p) != nil || p.Version != "ai-sdlc.publication-proof/v1" || p.PublicationID != c.ID || p.PlanSHA256 != c.PlanDigest || !modelDigest.MatchString(p.EffectsSHA256) || p.MergeAuthorized || p.DeploymentAuthorized {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		if lockPublication(ctx, tx, c) != nil {
			return ErrExecution
		}
		effects, e := readPublicationEffects(ctx, tx, c.ID)
		if e != nil || len(effects) != 4 {
			return ErrExecution
		}
		raw, _ := json.Marshal(effects)
		if authority.Digest(raw) != p.EffectsSHA256 {
			return ErrExecution
		}
		for i, kind := range []string{"tree", "commit", "branch", "pull-request"} {
			if effects[i].Kind != kind {
				return ErrExecution
			}
		}
		commit, branch, pr := effects[1].Receipt, effects[2].Receipt, effects[3].Receipt
		if commit.SHA != branch.SHA || commit.SHA != pr.SHA || commit.SHA != p.CommitSHA || branch.Branch != pr.Branch || branch.Branch != p.Branch || pr.PullRequest != p.PullRequest {
			return ErrExecution
		}
		if _, e = tx.ExecContext(ctx, `UPDATE publication_requests SET state='published',proof=$2 WHERE id=$1`, c.ID, proof); e != nil {
			return ErrExecution
		}
		return checkPublicationTime(ctx, tx, c)
	})
}
