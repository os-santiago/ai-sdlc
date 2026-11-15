package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

func (db *DB) ReadBehaviorProof(ctx context.Context, id string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := AssertComponent(ctx, db.Pool, "verifier"); err != nil {
		return nil, identityObservationError(err)
	}
	var raw []byte
	if err := db.Pool.QueryRowContext(ctx, `SELECT proof FROM verification_requests WHERE id=$1 AND state='passed' AND cleanup_state='complete' AND expires_at>clock_timestamp()`, id).Scan(&raw); err != nil {
		return nil, observationError(err)
	}
	if len(raw) < 1 || len(raw) > 65536 {
		return nil, ErrExecution
	}
	return raw, nil
}
func (db *DB) NextVerifiedForReview(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := AssertComponent(ctx, db.Pool, "verifier"); err != nil {
		return "", identityObservationError(err)
	}
	var id string
	err := db.Pool.QueryRowContext(ctx, `SELECT v.id FROM verification_requests v WHERE v.state='passed' AND v.cleanup_state='complete' AND v.expires_at>clock_timestamp() AND NOT EXISTS(SELECT 1 FROM review_runs r WHERE r.verification_id=v.id) ORDER BY v.created_at,v.id LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", observationError(err)
	}
	return id, nil
}

// FinishReview is only the final controller-owned commit. The controller must
// already have checked current signed evidence, exact candidate source, strict
// verdict and independently derived provider observations. It never publishes.
func (db *DB) FinishReview(ctx context.Context, c ReviewClaim, proof []byte) error {
	if len(proof) < 1 || len(proof) > 65536 || !json.Valid(proof) {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var header struct {
		Version               string `json:"version"`
		ReviewID              string `json:"reviewID"`
		VerificationID        string `json:"verificationID"`
		CandidateID           string `json:"candidateID"`
		BehaviorSHA256        string `json:"behaviorSHA256"`
		ProfileSHA256         string `json:"profileSHA256"`
		ConfigurationSHA256   string `json:"configurationSHA256"`
		AssignmentSHA256      string `json:"assignmentSHA256"`
		CandidateTreeSHA256   string `json:"candidateTreeSHA256"`
		ModelEvidenceSHA256   string `json:"modelEvidenceSHA256"`
		Verdict               string `json:"verdict"`
		SourceUnchanged       bool   `json:"sourceUnchanged"`
		PublicationAuthorized bool   `json:"publicationAuthorized"`
	}
	if authority.DecodeStrict(proof, &header) != nil || header.Version != "ai-sdlc.independent-review-proof/v1" || header.ReviewID != c.ID || header.Verdict != "approve" || !header.SourceUnchanged || header.PublicationAuthorized || !modelDigest.MatchString(header.ModelEvidenceSHA256) {
		return ErrExecution
	}
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		if lockReview(ctx, tx, c) != nil {
			return ErrExecution
		}
		var r ReviewRun
		var clean bool
		var behavior []byte
		if tx.QueryRowContext(ctx, `SELECT r.verification_id,r.candidate_id,r.behavior_digest,r.profile_digest,r.configuration_digest,r.assignment_digest,r.cleanup_state='complete',v.proof FROM review_runs r JOIN verification_requests v ON v.id=r.verification_id WHERE r.id=$1 AND v.state='passed' AND v.cleanup_state='complete' AND v.expires_at>clock_timestamp() FOR SHARE OF v`, c.ID).Scan(&r.VerificationID, &r.CandidateID, &r.BehaviorDigest, &r.ProfileDigest, &r.ConfigurationDigest, &r.AssignmentDigest, &clean, &behavior) != nil || !clean || r.VerificationID != header.VerificationID || r.CandidateID != header.CandidateID || r.BehaviorDigest != header.BehaviorSHA256 || authority.Digest(behavior) != r.BehaviorDigest || r.ProfileDigest != header.ProfileSHA256 || r.ConfigurationDigest != header.ConfigurationSHA256 || r.AssignmentDigest != header.AssignmentSHA256 {
			return ErrExecution
		}
		// Bind the imported/exported candidate tree; all five effects must have their
		// original UID receipt and no unknown/no-send outcome may claim success.
		var effects int
		var imported, exported string
		if tx.QueryRowContext(ctx, `SELECT count(*) FROM review_effects WHERE run_id=$1 AND resource_uid IS NOT NULL AND completed_at IS NOT NULL AND not_sent_at IS NULL`, c.ID).Scan(&effects) != nil || effects != 5 {
			return ErrExecution
		}
		if tx.QueryRowContext(ctx, `SELECT a.digest,b.digest FROM review_effects a JOIN review_effects b ON b.run_id=a.run_id WHERE a.run_id=$1 AND a.kind='import' AND b.kind='export' AND a.resource_uid=b.resource_uid`, c.ID).Scan(&imported, &exported) != nil || imported != exported || imported != header.CandidateTreeSHA256 {
			return ErrExecution
		}
		var candidateTree string
		if tx.QueryRowContext(ctx, `SELECT candidate_tree_sha256 FROM execution_candidates WHERE request_id=$1`, r.CandidateID).Scan(&candidateTree) != nil || candidateTree != imported {
			return ErrExecution
		}
		rows, e := tx.QueryContext(ctx, `SELECT sequence,request_digest,response_digest,input_bytes,output_bytes,observation FROM review_model_calls WHERE run_id=$1 AND state='handed-off' ORDER BY sequence`, c.ID)
		if e != nil {
			return ErrExecution
		}
		evidence := []ReviewModelEvidence{}
		for rows.Next() {
			var v ReviewModelEvidence
			if rows.Scan(&v.Sequence, &v.RequestDigest, &v.ResponseDigest, &v.InputBytes, &v.OutputBytes, &v.Observation) != nil {
				rows.Close()
				return ErrExecution
			}
			evidence = append(evidence, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil || len(evidence) < 2 || len(evidence) > 8 {
			return ErrExecution
		}
		raw, _ := json.Marshal(evidence)
		if authority.Digest(raw) != header.ModelEvidenceSHA256 {
			return ErrExecution
		}
		var calls int
		if tx.QueryRowContext(ctx, `SELECT reserved_calls FROM review_runs WHERE id=$1`, c.ID).Scan(&calls) != nil || calls != len(evidence) {
			return ErrExecution
		}
		for i, v := range evidence {
			if v.Sequence != i+1 {
				return ErrExecution
			}
			var observation struct {
				Verdict string `json:"verdict"`
			}
			if json.Unmarshal(v.Observation, &observation) != nil || (i == len(evidence)-1 && observation.Verdict != "approve") || (i < len(evidence)-1 && observation.Verdict != "") {
				return ErrExecution
			}
		}
		if _, e = tx.ExecContext(ctx, `UPDATE review_runs SET state='passed',proof=$2 WHERE id=$1`, c.ID, proof); e != nil {
			return ErrExecution
		}
		return checkReviewTime(ctx, tx, c)
	})
}

func (db *DB) RejectQueuedReview(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return ErrExecution
	}
	_, e := db.Pool.ExecContext(ctx, `UPDATE review_runs SET state='failed',reason='rejected',cleanup_state='complete' WHERE id=$1 AND state='queued'`, id)
	if e != nil {
		return ErrExecution
	}
	return nil
}
