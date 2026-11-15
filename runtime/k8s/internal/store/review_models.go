package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

type ReviewModelCall struct {
	Claim                        ReviewClaim
	Sequence                     int
	Token, PodUID, RequestDigest string
}
type ReviewModelEvidence struct {
	Sequence                      int
	RequestDigest, ResponseDigest string
	InputBytes, OutputBytes       int
	Observation                   []byte
}

// BeginReviewModelCall returns a reservation ONLY after a fresh durable insert
// commits. Current signed authority must still be checked before dispatch.
// Existing/uncertain calls are never adopted, replayed or refunded.
func (db *DB) BeginReviewModelCall(ctx context.Context, c ReviewClaim, pod string, sequence int, request []byte) (ReviewModelCall, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	call := ReviewModelCall{Claim: c, Sequence: sequence, Token: NewID(), PodUID: pod, RequestDigest: authority.Digest(request)}
	if !modelUUID.MatchString(pod) || sequence < 1 || sequence > 8 || len(request) < 1 || len(request) > 131072 || !json.Valid(request) {
		return ReviewModelCall{}, ErrExecution
	}
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if lockReview(ctx, tx, c) != nil || reviewPending(ctx, tx, c) != nil {
			return ErrExecution
		}
		var original string
		if tx.QueryRowContext(ctx, `SELECT resource_uid FROM review_effects WHERE run_id=$1 AND kind='start' AND completed_at IS NOT NULL`, c.ID).Scan(&original) != nil || original != pod {
			return ErrExecution
		}
		var calls, used int
		var blocked bool
		if tx.QueryRowContext(ctx, `SELECT reserved_calls,reserved_input_bytes,EXISTS(SELECT 1 FROM review_model_calls m WHERE m.run_id=$1 AND m.state<>'handed-off') OR EXISTS(SELECT 1 FROM review_effects WHERE run_id=$1 AND kind='export') FROM review_runs WHERE id=$1`, c.ID).Scan(&calls, &used, &blocked) != nil || blocked || calls+1 != sequence || used+len(request) > 1048576 {
			return ErrExecution
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO review_model_calls(run_id,sequence,token,pod_uid,request_digest,input_bytes) VALUES($1,$2,$3,$4,$5,$6)`, c.ID, sequence, call.Token, pod, call.RequestDigest, len(request)); e != nil {
			return ErrExecution
		}
		if _, e := tx.ExecContext(ctx, `UPDATE review_runs SET reserved_calls=reserved_calls+1,reserved_input_bytes=reserved_input_bytes+$2 WHERE id=$1`, c.ID, len(request)); e != nil {
			return ErrExecution
		}
		return checkReviewTime(ctx, tx, c)
	})
	if err != nil {
		return ReviewModelCall{}, ErrExecution
	}
	return call, nil
}

// CompleteReviewModelCall stores bounded trusted-controller observations, not
// source-authored SCC summaries. No response body is retained in the journal.
func (db *DB) CompleteReviewModelCall(ctx context.Context, c ReviewModelCall, response, observation []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if len(response) < 1 || len(response) > 32768 || !json.Valid(response) || len(observation) < 1 || len(observation) > 4096 || !json.Valid(observation) {
		return ErrExecution
	}
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		if lockReview(ctx, tx, c.Claim) != nil || reviewPending(ctx, tx, c.Claim) != nil {
			return ErrExecution
		}
		r, e := tx.ExecContext(ctx, `UPDATE review_model_calls SET state='completed',response_digest=$6,output_bytes=$7,observation=$8 WHERE run_id=$1 AND sequence=$2 AND token=$3 AND pod_uid=$4 AND request_digest=$5 AND state='started'`, c.Claim.ID, c.Sequence, c.Token, c.PodUID, c.RequestDigest, authority.Digest(response), len(response), observation)
		if e != nil {
			return ErrExecution
		}
		n, e := r.RowsAffected()
		if e != nil || n != 1 {
			return ErrExecution
		}
		return checkReviewTime(ctx, tx, c.Claim)
	})
}
func (db *DB) AcknowledgeReviewModelCall(ctx context.Context, c ReviewModelCall, responseDigest string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if !modelDigest.MatchString(responseDigest) {
		return ErrExecution
	}
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		if lockReview(ctx, tx, c.Claim) != nil || reviewPending(ctx, tx, c.Claim) != nil {
			return ErrExecution
		}
		r, e := tx.ExecContext(ctx, `UPDATE review_model_calls SET state='handed-off',handed_off_at=clock_timestamp() WHERE run_id=$1 AND sequence=$2 AND token=$3 AND pod_uid=$4 AND request_digest=$5 AND state='completed' AND response_digest=$6 AND handed_off_at IS NULL`, c.Claim.ID, c.Sequence, c.Token, c.PodUID, c.RequestDigest, responseDigest)
		if e != nil {
			return ErrExecution
		}
		n, e := r.RowsAffected()
		if e != nil || n != 1 {
			return ErrExecution
		}
		return checkReviewTime(ctx, tx, c.Claim)
	})
}
func (db *DB) ReadReviewModelEvidence(ctx context.Context, c ReviewClaim) ([]ReviewModelEvidence, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var result []ReviewModelEvidence
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if lockReview(ctx, tx, c) != nil {
			return ErrExecution
		}
		rows, e := tx.QueryContext(ctx, `SELECT sequence,request_digest,COALESCE(response_digest,''),input_bytes,COALESCE(output_bytes,0),observation,state FROM review_model_calls WHERE run_id=$1 ORDER BY sequence LIMIT 9`, c.ID)
		if e != nil {
			return ErrExecution
		}
		defer rows.Close()
		for rows.Next() {
			var v ReviewModelEvidence
			var state string
			if rows.Scan(&v.Sequence, &v.RequestDigest, &v.ResponseDigest, &v.InputBytes, &v.OutputBytes, &v.Observation, &state) != nil || state != "handed-off" || v.Sequence != len(result)+1 {
				return ErrExecution
			}
			result = append(result, v)
		}
		if rows.Err() != nil || len(result) < 1 || len(result) > 8 {
			return ErrExecution
		}
		return nil
	})
	if err != nil {
		return nil, ErrExecution
	}
	return result, nil
}
