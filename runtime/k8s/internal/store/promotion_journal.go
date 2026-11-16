package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
)

var promotionID = regexp.MustCompile(`^promote-[a-f0-9]{64}$`)

type PromotionJournal struct {
	*DB
	load func() (releasing.Configuration, error)
}

func NewPromotionJournal(db *DB, load func() (releasing.Configuration, error)) *PromotionJournal {
	return &PromotionJournal{db, load}
}

// The loader returns an owned snapshot. Copy its maps/slices before retaining
// configuration across SQL waits or returning it to a release manager.
func (j *PromotionJournal) configuration() (releasing.Configuration, error) {
	var out releasing.Configuration
	if j == nil || j.DB == nil || j.load == nil {
		return out, ErrExecution
	}
	cfg, e := j.load()
	if e != nil {
		return out, ErrExecution
	}
	raw, e := json.Marshal(cfg)
	if e != nil || len(raw) > 327680 || authority.DecodeStrict(raw, &out) != nil {
		return releasing.Configuration{}, ErrExecution
	}
	return out, nil
}
func (j *PromotionJournal) stillCurrent(ctx context.Context, c releasing.Command, r PromotionInput) error {
	cfg, e := j.configuration()
	if e != nil || ctx.Err() != nil || c.BindRecords(cfg, r.Provenance, r.Build, time.Now()) != nil {
		return ErrExecution
	}
	return nil
}
func (j *PromotionJournal) current(payload, receipt []byte) (releasing.Configuration, releasing.Command, error) {
	var cfg releasing.Configuration
	var c releasing.Command
	if j == nil || j.DB == nil || j.load == nil || len(receipt) == 0 || len(receipt) > 4096 {
		return cfg, c, ErrExecution
	}
	cfg, e := j.configuration()
	if e != nil {
		return cfg, c, ErrExecution
	}
	var signed authority.Receipt
	if authority.DecodeStrict(receipt, &signed) != nil {
		return cfg, c, ErrExecution
	}
	c, e = releasing.VerifyCommand(cfg, signed, payload, time.Now())
	if e != nil {
		return cfg, c, ErrExecution
	}
	return cfg, c, nil
}

type PromotionInput struct {
	Command    releasing.Command
	Provenance ProvenanceRecord
	Build      BuiltRecord
}

func (j *PromotionJournal) fresh(ctx context.Context, q Querier, cfg releasing.Configuration, c releasing.Command) (PromotionInput, error) {
	p, e := readProvenanceRecord(ctx, q, c.ProvenanceID, "release-manager")
	if e != nil {
		return PromotionInput{}, ErrExecution
	}
	b, e := readBuiltRecord(ctx, q, p.Command.BuildID, "release-manager")
	r := PromotionInput{c, p, b}
	if e != nil || c.BindRecords(cfg, p, b, time.Now()) != nil || freshBuildInput(ctx, q, cfg.Provenance.Build, b.Command, "release-manager") != nil || j.stillCurrent(ctx, c, r) != nil {
		return PromotionInput{}, ErrExecution
	}
	return r, nil
}

// QueuePromotionTx is only an authenticated inbox effect. It creates no network
// request and cannot accept a second command for the same completed build.
func (j *PromotionJournal) QueuePromotionTx(ctx context.Context, tx *sql.Tx, payload, receipt []byte) error {
	if tx == nil || AssertComponent(ctx, tx, "release-manager") != nil {
		return ErrExecution
	}
	cfg, c, e := j.current(payload, receipt)
	if e != nil {
		return e
	}
	if _, e = j.fresh(ctx, tx, cfg, c); e != nil {
		return e
	}
	config, _ := json.Marshal(cfg)
	result, e := tx.ExecContext(ctx, `INSERT INTO promotion_requests(id,provenance_id,payload_digest,payload,receipt,configuration,expires_at) SELECT $1,$2,$3,$4,$5,$6,$7 WHERE $7>clock_timestamp() AND $7<=clock_timestamp()+interval '125 seconds'`, c.ID(), c.ProvenanceID, authority.Digest(payload), payload, receipt, config, time.UnixMilli(c.ExpiresAt))
	if e != nil {
		return ErrExecution
	}
	n, e := result.RowsAffected()
	if e != nil || n != 1 {
		return ErrExecution
	}
	cfg, c, e = j.current(payload, receipt)
	if e != nil {
		return e
	}
	_, e = j.fresh(ctx, tx, cfg, c)
	return e
}
func (j *PromotionJournal) QueuePromotion(ctx context.Context, payload, receipt []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.DB.WithTx(ctx, func(tx *sql.Tx) error { return j.QueuePromotionTx(ctx, tx, payload, receipt) })
}

type PromotionClaim struct {
	ID, Token string
	Until     time.Time
}

// A lost claim acknowledgement leaves the attempt consumed. Restart may expire
// it but cannot repeat decision work under a new token.
func (j *PromotionJournal) ClaimPromotion(ctx context.Context, id string) (PromotionClaim, error) {
	c := PromotionClaim{ID: id, Token: NewID()}
	if !promotionID.MatchString(id) {
		return PromotionClaim{}, ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "release-manager") != nil {
			return ErrExecution
		}
		if tx.QueryRowContext(ctx, `UPDATE promotion_requests SET state='claimed',claim_token=$2 WHERE id=$1 AND state='queued' AND expires_at>clock_timestamp()+interval '10 seconds' RETURNING expires_at`, id, c.Token).Scan(&c.Until) != nil {
			return ErrExecution
		}
		_, _, _, e := j.check(ctx, tx, c)
		return e
	})
	if e != nil {
		return PromotionClaim{}, ErrExecution
	}
	// Do not expose a consumed claim if authority changed while COMMIT waited.
	if _, _, e = j.ReadPromotionInput(ctx, c); e != nil {
		return PromotionClaim{}, ErrExecution
	}
	return c, nil
}
func (j *PromotionJournal) check(ctx context.Context, tx *sql.Tx, c PromotionClaim) (releasing.Configuration, releasing.Command, PromotionInput, error) {
	var cfg releasing.Configuration
	var cmd releasing.Command
	var r PromotionInput
	if !promotionID.MatchString(c.ID) || !lifecycleUUID.MatchString(c.Token) || AssertComponent(ctx, tx, "release-manager") != nil {
		return cfg, cmd, r, ErrExecution
	}
	var raw, signed []byte
	var digest string
	var until time.Time
	if tx.QueryRowContext(ctx, `SELECT payload,receipt,payload_digest,expires_at FROM promotion_requests WHERE id=$1 AND claim_token=$2 AND state='claimed' FOR UPDATE`, c.ID, c.Token).Scan(&raw, &signed, &digest, &until) != nil || authority.Digest(raw) != digest || !until.Equal(c.Until) {
		return cfg, cmd, r, ErrExecution
	}
	cfg, cmd, e := j.current(raw, signed)
	if e != nil || cmd.ID() != c.ID || !until.Equal(time.UnixMilli(cmd.ExpiresAt)) {
		return cfg, cmd, r, ErrExecution
	}
	// Check database time after any row-lock wait as well as the command clock.
	var alive bool
	if tx.QueryRowContext(ctx, `SELECT $1>clock_timestamp()`, until).Scan(&alive) != nil || !alive {
		return cfg, cmd, r, ErrExecution
	}
	r, e = j.fresh(ctx, tx, cfg, cmd)
	return cfg, cmd, r, e
}

// ReadPromotionInput fences the exact authenticated record before bounded deterministic planning.
func (j *PromotionJournal) ReadPromotionInput(ctx context.Context, c PromotionClaim) (releasing.Configuration, PromotionInput, error) {
	var cfg releasing.Configuration
	var r PromotionInput
	var cmd releasing.Command
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := j.DB.WithTx(ctx, func(tx *sql.Tx) error { var e error; cfg, cmd, r, e = j.check(ctx, tx, c); return e })
	if e != nil || j.stillCurrent(ctx, cmd, r) != nil {
		return releasing.Configuration{}, PromotionInput{}, ErrExecution
	}
	return cfg, r, nil
}
func (j *PromotionJournal) FinishPromotion(ctx context.Context, c PromotionClaim, p releasing.Plan) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		cfg, cmd, r, e := j.check(ctx, tx, c)
		if e != nil || p.Validate(cfg, cmd, r.Provenance, r.Build) != nil || p.CompiledAt > time.Now().UnixMilli() || p.CompiledAt < cmd.IssuedAt {
			return ErrExecution
		}
		raw, _ := json.Marshal(p)
		if _, e = tx.ExecContext(ctx, `INSERT INTO promotion_records(request_id,digest,plan) VALUES($1,$2,$3)`, c.ID, authority.Digest(raw), raw); e != nil {
			return ErrExecution
		}
		// Repeat installed policy/trust/issue checks after writing, before commit.
		if _, _, _, e = j.check(ctx, tx, c); e != nil {
			return e
		}
		result, e := tx.ExecContext(ctx, `UPDATE promotion_requests SET state='planned' WHERE id=$1 AND claim_token=$2 AND state='claimed' AND expires_at>clock_timestamp()`, c.ID, c.Token)
		if e != nil {
			return ErrExecution
		}
		n, e := result.RowsAffected()
		if e != nil || n != 1 {
			return ErrExecution
		}
		return j.stillCurrent(ctx, cmd, r)
	})
}
func (j *PromotionJournal) StopPromotion(ctx context.Context, c PromotionClaim, reason string) error {
	if reason != "expired" && reason != "revoked" && reason != "uncertain" && reason != "rejected" {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, j.Pool, "release-manager") != nil || !promotionID.MatchString(c.ID) || !lifecycleUUID.MatchString(c.Token) {
		return ErrExecution
	}
	result, e := j.Pool.ExecContext(ctx, `UPDATE promotion_requests SET state='failed',reason=$3 WHERE id=$1 AND claim_token=$2 AND state='claimed'`, c.ID, c.Token, reason)
	if e != nil {
		return ErrExecution
	}
	n, e := result.RowsAffected()
	if e != nil || n != 1 {
		return ErrExecution
	}
	return nil
}
func (j *PromotionJournal) ExpirePromotion(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, j.Pool, "release-manager") != nil {
		return ErrExecution
	}
	_, e := j.Pool.ExecContext(ctx, `UPDATE promotion_requests SET state='failed',reason='expired' WHERE state IN ('queued','claimed') AND expires_at<=clock_timestamp()`)
	if e != nil {
		return ErrExecution
	}
	return nil
}
