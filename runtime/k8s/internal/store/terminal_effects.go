package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasecompletion"
	"time"
)

var terminalSteps = []string{"tag", "draft", "publish", "close"}

type TerminalProgress struct {
	NextStep  string
	ReleaseID int64
	Uncertain bool
	Receipts  []releasecompletion.EffectReceipt
}

func terminalProgress(ctx context.Context, q Querier, c TerminalClaim, p releasecompletion.Plan) (TerminalProgress, error) {
	var out TerminalProgress
	var previous time.Time
	for _, step := range terminalSteps {
		var request, raw []byte
		var intentHash, receiptHash, intentToken, sendToken string
		var intentAt, sentAt, recorded time.Time
		e := q.QueryRowContext(ctx, `SELECT i.request,i.request_digest,i.claim_token,i.created_at,s.claim_token,s.created_at,r.digest,r.receipt,r.recorded_at FROM terminal_intents i JOIN terminal_sends s ON s.request_id=i.request_id AND s.step=i.step JOIN terminal_receipts r ON r.request_id=s.request_id AND r.step=s.step WHERE i.request_id=$1 AND i.step=$2`, c.ID, step).Scan(&request, &intentHash, &intentToken, &intentAt, &sendToken, &sentAt, &receiptHash, &raw, &recorded)
		if e == sql.ErrNoRows {
			out.NextStep = step
			// A consumed fence with no original ACK is never retried, even if an
			// external object happens to match the desired result.
			if q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM terminal_sends WHERE request_id=$1 AND step=$2)`, c.ID, step).Scan(&out.Uncertain) != nil {
				return out, ErrExecution
			}
			return out, nil
		}
		if e != nil || intentToken != c.Token || sendToken != c.Token || intentAt.Before(previous) || sentAt.Before(intentAt) || !recorded.Before(c.Until) || recorded.After(time.Now().Add(5*time.Second)) {
			return out, ErrExecution
		}
		var req releasecompletion.EffectRequest
		var receipt releasecompletion.EffectReceipt
		if authority.DecodeStrict(request, &req) != nil || req.Step != step || req.Digest() != intentHash || authority.Digest(raw) != receiptHash || authority.DecodeStrict(raw, &receipt) != nil || receipt.Step != step || releasecompletion.ValidateReceipt(p, req, out.ReleaseID, receipt, sentAt, recorded) != nil {
			return out, ErrExecution
		}
		// The next request's release ID is solely the acknowledged draft ID.
		if step == "draft" {
			out.ReleaseID = receipt.ReleaseID
		}
		out.Receipts = append(out.Receipts, receipt)
		previous = recorded
	}
	return out, nil
}
func (j *TerminalJournal) ReadTerminalProgress(ctx context.Context, c TerminalClaim) (TerminalProgress, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, e := terminalArchived(ctx, j.Pool, c)
	if e != nil {
		return TerminalProgress{}, e
	}
	return terminalProgress(ctx, j.Pool, c, r.Plan)
}
func terminalExpected(ctx context.Context, q Querier, c TerminalClaim, p releasecompletion.Plan, step string) (releasecompletion.EffectRequest, error) {
	progress, e := terminalProgress(ctx, q, c, p)
	if e != nil || progress.NextStep != step || progress.Uncertain {
		return releasecompletion.EffectRequest{}, ErrExecution
	}
	req, e := releasecompletion.Request(p, step, progress.ReleaseID, time.Now())
	if e != nil {
		return req, ErrExecution
	}
	return req, nil
}
func (j *TerminalJournal) BeginTerminalEffect(ctx context.Context, c TerminalClaim, step string) (releasecompletion.EffectRequest, error) {
	var request releasecompletion.EffectRequest
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := j.WithTx(ctx, func(tx *sql.Tx) error {
		r, e := j.check(ctx, tx, c, true)
		if e != nil {
			return e
		}
		request, e = terminalExpected(ctx, tx, c, r.Plan, step)
		if e != nil {
			return e
		}
		raw, _ := json.Marshal(request)
		var existing []byte
		var hash, token string
		e = tx.QueryRowContext(ctx, `SELECT request,request_digest,claim_token FROM terminal_intents WHERE request_id=$1 AND step=$2`, c.ID, step).Scan(&existing, &hash, &token)
		if e == sql.ErrNoRows {
			_, e = tx.ExecContext(ctx, `INSERT INTO terminal_intents(request_id,step,claim_token,request,request_digest)VALUES($1,$2,$3,$4,$5)`, c.ID, step, c.Token, raw, request.Digest())
		} else if e == nil && (string(existing) != string(raw) || hash != request.Digest() || token != c.Token) {
			return ErrExecution
		}
		if e != nil {
			return ErrExecution
		}
		r, e = j.check(ctx, tx, c, true)
		if e != nil {
			return e
		}
		expected, e := terminalExpected(ctx, tx, c, r.Plan, step)
		if e != nil || expected.Digest() != request.Digest() {
			return ErrExecution
		}
		return nil
	})
	if e != nil {
		return releasecompletion.EffectRequest{}, ErrExecution
	}
	return request, nil
}

// SendTerminalEffect is invoked once by the transport immediately before a
// mutation. A lost COMMIT acknowledgement denies sending; recovery quarantines
// a durable send without an ACK. It never grants a replacement send.
func (j *TerminalJournal) SendTerminalEffect(ctx context.Context, c TerminalClaim, request releasecompletion.EffectRequest) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := j.WithTx(ctx, func(tx *sql.Tx) error {
		r, e := j.check(ctx, tx, c, true)
		if e != nil {
			return e
		}
		expected, e := terminalExpected(ctx, tx, c, r.Plan, request.Step)
		if e != nil || expected.Digest() != request.Digest() {
			return ErrExecution
		}
		var hash, token string
		var raw []byte
		if tx.QueryRowContext(ctx, `SELECT request_digest,claim_token,request FROM terminal_intents WHERE request_id=$1 AND step=$2`, c.ID, request.Step).Scan(&hash, &token, &raw) != nil || hash != request.Digest() || token != c.Token {
			return ErrExecution
		}
		var saved releasecompletion.EffectRequest
		if authority.DecodeStrict(raw, &saved) != nil || saved.Digest() != request.Digest() {
			return ErrExecution
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO terminal_sends(request_id,step,claim_token)VALUES($1,$2,$3)`, c.ID, request.Step, c.Token); e != nil {
			return ErrExecution
		}
		_, e = j.check(ctx, tx, c, true)
		return e
	})
	if e != nil {
		return ErrExecution
	}
	// COMMIT can block. Recheck current authority and eligibility before returning
	// permission to the transport, without consuming another send.
	return j.WithTx(ctx, func(tx *sql.Tx) error { _, e := j.check(ctx, tx, c, true); return e })
}
func terminalReceiptInput(ctx context.Context, q Querier, c TerminalClaim, r TerminalInput, receipt releasecompletion.EffectReceipt) (releasecompletion.EffectRequest, int64, time.Time, error) {
	progress, e := terminalProgress(ctx, q, c, r.Plan)
	if e != nil || progress.NextStep != receipt.Step || !progress.Uncertain {
		return releasecompletion.EffectRequest{}, 0, time.Time{}, ErrExecution
	}
	var raw []byte
	var hash, token, sendToken string
	var intentAt, sent time.Time
	if q.QueryRowContext(ctx, `SELECT i.request,i.request_digest,i.claim_token,i.created_at,s.claim_token,s.created_at FROM terminal_intents i JOIN terminal_sends s ON s.request_id=i.request_id AND s.step=i.step WHERE i.request_id=$1 AND i.step=$2`, c.ID, receipt.Step).Scan(&raw, &hash, &token, &intentAt, &sendToken, &sent) != nil || token != c.Token || sendToken != c.Token || sent.Before(intentAt) {
		return releasecompletion.EffectRequest{}, 0, time.Time{}, ErrExecution
	}
	var request releasecompletion.EffectRequest
	if authority.DecodeStrict(raw, &request) != nil || request.Digest() != hash {
		return request, 0, sent, ErrExecution
	}
	return request, progress.ReleaseID, sent, nil
}
func (j *TerminalJournal) RecordTerminalReceipt(ctx context.Context, c TerminalClaim, receipt releasecompletion.EffectReceipt) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.WithTx(ctx, func(tx *sql.Tx) error {
		r, e := j.check(ctx, tx, c, false)
		if e != nil {
			return e
		}
		request, id, sent, e := terminalReceiptInput(ctx, tx, c, r, receipt)
		if e != nil || releasecompletion.ValidateReceipt(r.Plan, request, id, receipt, sent, time.Now()) != nil {
			return ErrExecution
		}
		raw, _ := json.Marshal(receipt)
		var recorded time.Time
		if tx.QueryRowContext(ctx, `INSERT INTO terminal_receipts(request_id,step,digest,receipt)SELECT $1,$2,$3,$4 WHERE clock_timestamp()<$5 RETURNING recorded_at`, c.ID, receipt.Step, authority.Digest(raw), raw, c.Until).Scan(&recorded) != nil || !recorded.Before(c.Until) || releasecompletion.ValidateReceipt(r.Plan, request, id, receipt, sent, recorded) != nil {
			return ErrExecution
		}
		progress, e := terminalProgress(ctx, tx, c, r.Plan)
		if e != nil || len(progress.Receipts) == 0 || progress.Receipts[len(progress.Receipts)-1] != receipt {
			return ErrExecution
		}
		return nil
	})
}
func (j *TerminalJournal) FinishTerminal(ctx context.Context, c TerminalClaim) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.WithTx(ctx, func(tx *sql.Tx) error {
		r, e := j.check(ctx, tx, c, false)
		if e != nil {
			return e
		}
		p, e := terminalProgress(ctx, tx, c, r.Plan)
		if e != nil || p.NextStep != "" || p.Uncertain || len(p.Receipts) != 4 {
			return ErrExecution
		}
		result, e := tx.ExecContext(ctx, `UPDATE terminal_requests SET state='completed',completed_at=clock_timestamp() WHERE id=$1 AND claim_token=$2 AND state='claimed'`, c.ID, c.Token)
		if e != nil {
			return ErrExecution
		}
		n, e := result.RowsAffected()
		if e != nil || n != 1 {
			return ErrExecution
		}
		return nil
	})
}
func (j *TerminalJournal) QuarantineTerminal(ctx context.Context, c TerminalClaim, reason string) error {
	if reason != "expired" && reason != "revoked" && reason != "uncertain" && reason != "rejected" {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if AssertComponent(ctx, j.Pool, "release-manager") != nil {
		return ErrExecution
	}
	_, e := j.Pool.ExecContext(ctx, `UPDATE terminal_requests SET state='quarantined',reason=$3 WHERE id=$1 AND claim_token=$2 AND state='claimed'`, c.ID, c.Token, reason)
	return e
}
func (j *TerminalJournal) NextTerminalRecovery(ctx context.Context) (TerminalClaim, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if AssertComponent(ctx, j.Pool, "release-manager") != nil {
		return TerminalClaim{}, ErrExecution
	}
	var c TerminalClaim
	e := j.Pool.QueryRowContext(ctx, `SELECT id,claim_token,expires_at FROM terminal_requests WHERE state='claimed' ORDER BY created_at,id LIMIT 1`).Scan(&c.ID, &c.Token, &c.Until)
	if e == sql.ErrNoRows {
		return TerminalClaim{}, nil
	}
	if e != nil {
		return TerminalClaim{}, ErrExecution
	}
	return c, nil
}
