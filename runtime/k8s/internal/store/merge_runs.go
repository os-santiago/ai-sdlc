package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/merging"
)

var mergeIDPattern = regexp.MustCompile(`^merge-[a-f0-9]{64}$`)

type MergeRequest struct {
	ID, PublicationID, RecordDigest, GateDigest, AuthorityRevision, PayloadDigest string
	Payload, Receipt, Gate                                                        []byte
	ExpiresAt                                                                     time.Time
}
type MergeClaim struct {
	ID, Token, RecordDigest, GateDigest string
	Until                               time.Time
}

// QueueMerge verifies fresh signed authority and binds the exact observed gate
// and immutable publication fact. It never accepts an expired generation claim.
func (j *MergeJournal) QueueMerge(ctx context.Context, r MergeRequest) error {
	if !mergeIDPattern.MatchString(r.ID) || !publicationIDPattern.MatchString(r.PublicationID) || len(r.Payload) < 1 || len(r.Payload) > 16384 || len(r.Receipt) < 1 || len(r.Receipt) > 4096 || authority.Digest(r.Payload) != r.PayloadDigest {
		return ErrExecution
	}
	for _, v := range []string{r.RecordDigest, r.GateDigest, r.AuthorityRevision} {
		if !modelDigest.MatchString(v) {
			return ErrExecution
		}
	}
	var command merging.Command
	if authority.DecodeStrict(r.Payload, &command) != nil || command.Version != merging.Version || command.ID() != r.ID || command.PublicationID != r.PublicationID || command.PublicationRecordSHA256 != r.RecordDigest || command.GateSHA256 != r.GateDigest || command.AuthorityRevision != r.AuthorityRevision || !time.UnixMilli(command.ExpiresAt).Equal(r.ExpiresAt) {
		return ErrExecution
	}
	policy, e := j.currentAuthority(r.Payload, r.Receipt, r.Gate, time.Now())
	if e != nil {
		return ErrExecution
	}
	archived, _ := json.Marshal(policy)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "release-manager") != nil {
			return ErrExecution
		}
		fact, e := readPublishedRecord(ctx, tx, r.PublicationID)
		if e != nil || fact.Digest() != r.RecordDigest {
			return ErrExecution
		}
		var proof PublicationProof
		var effects []PublicationEvidence
		var original struct {
			Repository  string
			IssueNumber int    `json:"issueNumber"`
			SourceSHA   string `json:"sourceSHA"`
		}
		if authority.DecodeStrict(fact.Proof, &proof) != nil || authority.DecodeStrict(fact.Effects, &effects) != nil || len(effects) != 4 || json.Unmarshal(fact.Payload, &original) != nil || command.Repository != original.Repository || command.IssueNumber != original.IssueNumber || command.BaseSHA != original.SourceSHA || command.HeadSHA != proof.CommitSHA || command.TreeSHA != effects[0].Receipt.SHA || command.Branch != proof.Branch || command.PullRequest != proof.PullRequest {
			return ErrExecution
		}

		result, e := tx.ExecContext(ctx, `INSERT INTO merge_requests(id,publication_id,record_digest,gate_digest,authority_revision,payload_digest,payload,receipt,expires_at,gate,policy) SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11 WHERE $9>clock_timestamp() AND $9<=clock_timestamp()+interval '120 seconds'`, r.ID, r.PublicationID, r.RecordDigest, r.GateDigest, r.AuthorityRevision, r.PayloadDigest, r.Payload, r.Receipt, r.ExpiresAt, r.Gate, archived)
		if e != nil {
			return ErrExecution
		}
		n, e := result.RowsAffected()
		if e != nil || n != 1 {
			return ErrExecution
		}
		_, e = j.currentAuthority(r.Payload, r.Receipt, r.Gate, time.Now())
		return e
	})
}
func (j *MergeJournal) ReadMerge(ctx context.Context, id string) (MergeRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var r MergeRequest
	if AssertComponent(ctx, j.DB.Pool, "release-manager") != nil {
		return r, ErrExecution
	}
	e := j.DB.Pool.QueryRowContext(ctx, `SELECT id,publication_id,record_digest,gate_digest,authority_revision,payload_digest,payload,receipt,gate,expires_at FROM merge_requests WHERE id=$1`, id).Scan(&r.ID, &r.PublicationID, &r.RecordDigest, &r.GateDigest, &r.AuthorityRevision, &r.PayloadDigest, &r.Payload, &r.Receipt, &r.Gate, &r.ExpiresAt)
	if e != nil {
		return MergeRequest{}, observationError(e)
	}
	return r, nil
}
func (j *MergeJournal) ClaimMerge(ctx context.Context, id string) (MergeClaim, error) {
	c := MergeClaim{ID: id, Token: NewID()}
	if !mergeIDPattern.MatchString(id) {
		return MergeClaim{}, ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "release-manager") != nil {
			return ErrExecution
		}
		if _, e := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(73421026)`); e != nil {
			return ErrExecution
		}
		var blocked bool
		if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM merge_requests WHERE state='claimed') OR EXISTS(SELECT 1 FROM merge_effects e JOIN merge_requests r ON r.id=e.request_id WHERE r.state='failed' AND e.completed_at IS NULL AND e.not_sent_at IS NULL)`).Scan(&blocked) != nil || blocked {
			return ErrExecution
		}
		if tx.QueryRowContext(ctx, `UPDATE merge_requests SET state='claimed',claim_token=$2,claim_until=LEAST(expires_at,clock_timestamp()+interval '90 seconds') WHERE id=$1 AND state='queued' AND expires_at>clock_timestamp() RETURNING record_digest,gate_digest,claim_until`, id, c.Token).Scan(&c.RecordDigest, &c.GateDigest, &c.Until) != nil {
			return ErrExecution
		}
		return j.checkMergeTime(ctx, tx, c)
	})
	if e != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = j.StopMerge(cleanup, c, "uncertain")
		return MergeClaim{}, ErrExecution
	}
	return c, nil
}
func (j *MergeJournal) checkMergeTime(ctx context.Context, tx *sql.Tx, c MergeClaim) error {
	var ok bool
	if tx.QueryRowContext(ctx, `SELECT claim_until>clock_timestamp() AND expires_at>clock_timestamp() FROM merge_requests WHERE id=$1 AND claim_token=$2`, c.ID, c.Token).Scan(&ok) != nil || !ok {
		return ErrExecution
	}
	var payload, receipt, gate []byte
	if tx.QueryRowContext(ctx, `SELECT payload,receipt,gate FROM merge_requests WHERE id=$1`, c.ID).Scan(&payload, &receipt, &gate) != nil {
		return ErrExecution
	}
	_, e := j.currentAuthority(payload, receipt, gate, time.Now())
	return e
}
func (j *MergeJournal) lockMerge(ctx context.Context, tx *sql.Tx, c MergeClaim) error {
	if AssertComponent(ctx, tx, "release-manager") != nil || !mergeIDPattern.MatchString(c.ID) || !lifecycleUUID.MatchString(c.Token) || !modelDigest.MatchString(c.RecordDigest) || !modelDigest.MatchString(c.GateDigest) {
		return ErrExecution
	}
	var id string
	if tx.QueryRowContext(ctx, `SELECT id FROM merge_requests WHERE id=$1 AND claim_token=$2 AND record_digest=$3 AND gate_digest=$4 AND state='claimed' FOR UPDATE`, c.ID, c.Token, c.RecordDigest, c.GateDigest).Scan(&id) != nil {
		return ErrExecution
	}
	// Observe wall time after acquiring the row lock, not in its SELECT expression.
	return j.checkMergeTime(ctx, tx, c)
}
func (j *MergeJournal) CheckMerge(ctx context.Context, c MergeClaim) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return j.DB.WithTx(ctx, func(tx *sql.Tx) error { return j.lockMerge(ctx, tx, c) })
}
func (j *MergeJournal) StopMerge(ctx context.Context, c MergeClaim, reason string) error {
	if reason != "expired" && reason != "revoked" && reason != "uncertain" && reason != "rejected" {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, j.DB.Pool, "release-manager") != nil {
		return ErrExecution
	}
	result, e := j.DB.Pool.ExecContext(ctx, `UPDATE merge_requests SET state='failed',reason=$3 WHERE id=$1 AND claim_token=$2 AND state='claimed'`, c.ID, c.Token, reason)
	if e != nil {
		return ErrExecution
	}
	n, e := result.RowsAffected()
	if e != nil || n != 1 {
		return ErrExecution
	}
	return nil
}
func (j *MergeJournal) ExpireMerges(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, j.DB.Pool, "release-manager") != nil {
		return ErrExecution
	}
	_, e := j.DB.Pool.ExecContext(ctx, `UPDATE merge_requests SET state='failed',reason='expired' WHERE state IN ('queued','claimed') AND (expires_at<=clock_timestamp() OR claim_until<=clock_timestamp())`)
	if e != nil {
		return ErrExecution
	}
	return nil
}
func (j *MergeJournal) MergeBlocked(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, j.DB.Pool, "release-manager") != nil {
		return false, ErrExecution
	}
	var blocked bool
	e := j.DB.Pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM merge_effects e JOIN merge_requests r ON r.id=e.request_id WHERE r.state='failed' AND e.completed_at IS NULL AND e.not_sent_at IS NULL)`).Scan(&blocked)
	if e != nil {
		return false, ErrExecution
	}
	return blocked, nil
}
func (j *MergeJournal) NextMerge(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, j.DB.Pool, "release-manager") != nil {
		return "", ErrExecution
	}
	var id string
	e := j.DB.Pool.QueryRowContext(ctx, `SELECT id FROM merge_requests WHERE state='queued' AND expires_at>clock_timestamp() ORDER BY created_at,id LIMIT 1`).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	if e != nil {
		return "", observationError(e)
	}
	return id, nil
}
