package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
)

var provenanceID = regexp.MustCompile(`^verify-[a-f0-9]{64}$`)

type ProvenanceJournal struct {
	*DB
	load func() (provenance.Configuration, error)
}

func NewProvenanceJournal(db *DB, load func() (provenance.Configuration, error)) *ProvenanceJournal {
	return &ProvenanceJournal{db, load}
}

// The loader returns an owned snapshot. Copy its maps/slices before retaining
// configuration across SQL waits or returning it to a verifier.
func (j *ProvenanceJournal) configuration() (provenance.Configuration, error) {
	var out provenance.Configuration
	if j == nil || j.DB == nil || j.load == nil {
		return out, ErrExecution
	}
	cfg, e := j.load()
	if e != nil {
		return out, ErrExecution
	}
	raw, e := json.Marshal(cfg)
	if e != nil || len(raw) > 196608 || authority.DecodeStrict(raw, &out) != nil {
		return provenance.Configuration{}, ErrExecution
	}
	return out, nil
}
func (j *ProvenanceJournal) stillCurrent(ctx context.Context, c provenance.Command, r BuiltRecord) error {
	cfg, e := j.configuration()
	if e != nil || ctx.Err() != nil || c.BindRecord(cfg, r, time.Now()) != nil {
		return ErrExecution
	}
	return nil
}
func (j *ProvenanceJournal) current(payload, receipt []byte) (provenance.Configuration, provenance.Command, error) {
	var cfg provenance.Configuration
	var c provenance.Command
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
	c, e = provenance.VerifyCommand(cfg, signed, payload, time.Now())
	if e != nil {
		return cfg, c, ErrExecution
	}
	return cfg, c, nil
}
func (j *ProvenanceJournal) fresh(ctx context.Context, q Querier, cfg provenance.Configuration, c provenance.Command) (BuiltRecord, error) {
	r, e := readBuiltRecord(ctx, q, c.BuildID, "verifier")
	if e != nil || c.BindRecord(cfg, r, time.Now()) != nil || freshBuildInput(ctx, q, cfg.Build, r.Command, "verifier") != nil || j.stillCurrent(ctx, c, r) != nil {
		return BuiltRecord{}, ErrExecution
	}
	return r, nil
}

// QueueProvenanceTx is only an authenticated inbox effect. It creates no network
// request and cannot accept a second command for the same completed build.
func (j *ProvenanceJournal) QueueProvenanceTx(ctx context.Context, tx *sql.Tx, payload, receipt []byte) error {
	if tx == nil || AssertComponent(ctx, tx, "verifier") != nil {
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
	result, e := tx.ExecContext(ctx, `INSERT INTO provenance_requests(id,build_id,payload_digest,payload,receipt,configuration,expires_at) SELECT $1,$2,$3,$4,$5,$6,$7 WHERE $7>clock_timestamp() AND $7<=clock_timestamp()+interval '125 seconds'`, c.ID(), c.BuildID, authority.Digest(payload), payload, receipt, config, time.UnixMilli(c.ExpiresAt))
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
func (j *ProvenanceJournal) QueueProvenance(ctx context.Context, payload, receipt []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.DB.WithTx(ctx, func(tx *sql.Tx) error { return j.QueueProvenanceTx(ctx, tx, payload, receipt) })
}

type ProvenanceClaim struct {
	ID, Token string
	Until     time.Time
}

// A lost claim acknowledgement leaves the attempt consumed. Restart may expire
// it but cannot repeat registry I/O under a new token.
func (j *ProvenanceJournal) ClaimProvenance(ctx context.Context, id string) (ProvenanceClaim, error) {
	c := ProvenanceClaim{ID: id, Token: NewID()}
	if !provenanceID.MatchString(id) {
		return ProvenanceClaim{}, ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "verifier") != nil {
			return ErrExecution
		}
		if tx.QueryRowContext(ctx, `UPDATE provenance_requests SET state='claimed',claim_token=$2 WHERE id=$1 AND state='queued' AND expires_at>clock_timestamp()+interval '65 seconds' RETURNING expires_at`, id, c.Token).Scan(&c.Until) != nil {
			return ErrExecution
		}
		_, _, _, e := j.check(ctx, tx, c)
		return e
	})
	if e != nil {
		return ProvenanceClaim{}, ErrExecution
	}
	// Do not expose a consumed claim if authority changed while COMMIT waited.
	if _, _, e = j.ReadProvenanceInput(ctx, c); e != nil {
		return ProvenanceClaim{}, ErrExecution
	}
	return c, nil
}
func (j *ProvenanceJournal) check(ctx context.Context, tx *sql.Tx, c ProvenanceClaim) (provenance.Configuration, provenance.Command, BuiltRecord, error) {
	var cfg provenance.Configuration
	var cmd provenance.Command
	var r BuiltRecord
	if !provenanceID.MatchString(c.ID) || !lifecycleUUID.MatchString(c.Token) || AssertComponent(ctx, tx, "verifier") != nil {
		return cfg, cmd, r, ErrExecution
	}
	var raw, signed []byte
	var digest string
	var until time.Time
	if tx.QueryRowContext(ctx, `SELECT payload,receipt,payload_digest,expires_at FROM provenance_requests WHERE id=$1 AND claim_token=$2 AND state='claimed' FOR UPDATE`, c.ID, c.Token).Scan(&raw, &signed, &digest, &until) != nil || authority.Digest(raw) != digest || !until.Equal(c.Until) {
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

// ReadProvenanceInput fences the exact authenticated record before bounded I/O.
func (j *ProvenanceJournal) ReadProvenanceInput(ctx context.Context, c ProvenanceClaim) (provenance.Configuration, BuiltRecord, error) {
	var cfg provenance.Configuration
	var r BuiltRecord
	var cmd provenance.Command
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := j.DB.WithTx(ctx, func(tx *sql.Tx) error { var e error; cfg, cmd, r, e = j.check(ctx, tx, c); return e })
	if e != nil || j.stillCurrent(ctx, cmd, r) != nil {
		return provenance.Configuration{}, BuiltRecord{}, ErrExecution
	}
	return cfg, r, nil
}
func (j *ProvenanceJournal) FinishProvenance(ctx context.Context, c ProvenanceClaim, p provenance.Proof) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		cfg, cmd, r, e := j.check(ctx, tx, c)
		if e != nil || cmd.BindProof(cfg, r, p, time.Now()) != nil {
			return ErrExecution
		}
		raw, _ := json.Marshal(p)
		if _, e = tx.ExecContext(ctx, `INSERT INTO provenance_records(request_id,digest,proof) VALUES($1,$2,$3)`, c.ID, authority.Digest(raw), raw); e != nil {
			return ErrExecution
		}
		// Repeat installed policy/trust/issue checks after writing, before commit.
		if _, _, _, e = j.check(ctx, tx, c); e != nil {
			return e
		}
		result, e := tx.ExecContext(ctx, `UPDATE provenance_requests SET state='verified' WHERE id=$1 AND claim_token=$2 AND state='claimed' AND expires_at>clock_timestamp()`, c.ID, c.Token)
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
func (j *ProvenanceJournal) StopProvenance(ctx context.Context, c ProvenanceClaim, reason string) error {
	if reason != "expired" && reason != "revoked" && reason != "uncertain" && reason != "rejected" {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, j.Pool, "verifier") != nil || !provenanceID.MatchString(c.ID) || !lifecycleUUID.MatchString(c.Token) {
		return ErrExecution
	}
	result, e := j.Pool.ExecContext(ctx, `UPDATE provenance_requests SET state='failed',reason=$3 WHERE id=$1 AND claim_token=$2 AND state='claimed'`, c.ID, c.Token, reason)
	if e != nil {
		return ErrExecution
	}
	n, e := result.RowsAffected()
	if e != nil || n != 1 {
		return ErrExecution
	}
	return nil
}
func (j *ProvenanceJournal) ExpireProvenance(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, j.Pool, "verifier") != nil {
		return ErrExecution
	}
	_, e := j.Pool.ExecContext(ctx, `UPDATE provenance_requests SET state='failed',reason='expired' WHERE state IN ('queued','claimed') AND expires_at<=clock_timestamp()`)
	if e != nil {
		return ErrExecution
	}
	return nil
}
