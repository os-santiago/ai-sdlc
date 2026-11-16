package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"regexp"
	"time"
)

var buildIDPattern = regexp.MustCompile(`^build-[a-f0-9]{64}$`)

type BuildClaim struct {
	ID, Token, SlotID, SlotDigest string
	Until                         time.Time
}

func (j *BuildJournal) QueueBuild(ctx context.Context, payload, receipt []byte) error {
	cfg, c, e := j.current(payload, receipt)
	if e != nil {
		return e
	}
	inner, _ := json.Marshal(cfg.Inner)
	outer, _ := json.Marshal(cfg.Outer)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "build-controller") != nil || j.fresh(ctx, tx, cfg, c) != nil {
			return ErrExecution
		}
		r, e := tx.ExecContext(ctx, `INSERT INTO build_requests(id,merge_id,payload_digest,payload,receipt,inner_policy,outer_policy,expires_at) SELECT $1,$2,$3,$4,$5,$6,$7,$8 WHERE $8>clock_timestamp() AND $8<=clock_timestamp()+interval '1200 seconds'`, c.ID(), c.MergeID, authority.Digest(payload), payload, receipt, inner, outer, time.UnixMilli(c.ExpiresAt))
		if e != nil {
			return ErrExecution
		}
		n, e := r.RowsAffected()
		if e != nil || n != 1 {
			return ErrExecution
		}
		_, _, e = j.current(payload, receipt)
		return e
	})
}
func (j *BuildJournal) ClaimBuild(ctx context.Context, id string, slot building.Slot) (BuildClaim, error) {
	c := BuildClaim{ID: id, Token: NewID(), SlotID: slot.ID(), SlotDigest: slot.Digest()}
	if !buildIDPattern.MatchString(id) || slot.Validate() != nil {
		return BuildClaim{}, ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "build-controller") != nil {
			return ErrExecution
		}
		var payload, receipt []byte
		if tx.QueryRowContext(ctx, `UPDATE build_requests SET state='claimed',claim_token=$2,claim_until=expires_at WHERE id=$1 AND state='queued' AND expires_at>clock_timestamp() RETURNING payload,receipt,claim_until`, id, c.Token).Scan(&payload, &receipt, &c.Until) != nil {
			return ErrExecution
		}
		cfg, cmd, e := j.current(payload, receipt)
		if e != nil || j.fresh(ctx, tx, cfg, cmd) != nil {
			return ErrExecution
		}
		allowed := false
		for _, s := range cfg.Slots {
			if s == slot {
				allowed = true
			}
		}
		if !allowed {
			return ErrExecution
		}
		raw, _ := json.Marshal(slot)
		if _, e = tx.ExecContext(ctx, `INSERT INTO build_slots(slot_id,request_id,slot_digest,slot) VALUES($1,$2,$3,$4)`, c.SlotID, id, c.SlotDigest, raw); e != nil {
			return ErrExecution
		}
		return j.check(ctx, tx, c)
	})
	if e != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = j.StopBuild(cleanup, c, "uncertain")
		return BuildClaim{}, ErrExecution
	}
	return c, nil
}
func (j *BuildJournal) check(ctx context.Context, q Querier, c BuildClaim) error {
	var payload, receipt, slotRaw []byte
	if q.QueryRowContext(ctx, `SELECT r.payload,r.receipt,s.slot FROM build_requests r JOIN build_slots s ON s.request_id=r.id WHERE r.id=$1 AND r.claim_token=$2 AND r.state='claimed' AND r.claim_until>clock_timestamp() AND r.expires_at>clock_timestamp() AND s.slot_id=$3 AND s.slot_digest=$4`, c.ID, c.Token, c.SlotID, c.SlotDigest).Scan(&payload, &receipt, &slotRaw) != nil {
		return ErrExecution
	}
	cfg, cmd, e := j.current(payload, receipt)
	if e != nil {
		return ErrExecution
	}
	var slot building.Slot
	if authority.DecodeStrict(slotRaw, &slot) != nil || slot.ID() != c.SlotID || slot.Digest() != c.SlotDigest {
		return ErrExecution
	}
	allowed := false
	for _, s := range cfg.Slots {
		if s == slot {
			allowed = true
		}
	}
	if !allowed {
		return ErrExecution
	}
	return j.fresh(ctx, q, cfg, cmd)
}
func (j *BuildJournal) lock(ctx context.Context, tx *sql.Tx, c BuildClaim) error {
	if AssertComponent(ctx, tx, "build-controller") != nil || !buildIDPattern.MatchString(c.ID) || !lifecycleUUID.MatchString(c.Token) {
		return ErrExecution
	}
	var id string
	if tx.QueryRowContext(ctx, `SELECT id FROM build_requests WHERE id=$1 AND claim_token=$2 AND state='claimed' FOR UPDATE`, c.ID, c.Token).Scan(&id) != nil {
		return ErrExecution
	}
	return j.check(ctx, tx, c)
}
func (j *BuildJournal) CheckBuild(ctx context.Context, c BuildClaim) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.DB.WithTx(ctx, func(tx *sql.Tx) error { return j.lock(ctx, tx, c) })
}
func (j *BuildJournal) StopBuild(ctx context.Context, c BuildClaim, reason string) error {
	if reason != "expired" && reason != "revoked" && reason != "uncertain" && reason != "rejected" {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, j.Pool, "build-controller") != nil {
		return ErrExecution
	}
	r, e := j.Pool.ExecContext(ctx, `UPDATE build_requests SET state='failed',reason=$3 WHERE id=$1 AND claim_token=$2 AND state='claimed'`, c.ID, c.Token, reason)
	if e != nil {
		return ErrExecution
	}
	n, e := r.RowsAffected()
	if e != nil || n != 1 {
		return ErrExecution
	}
	return nil
}
func (j *BuildJournal) ExpireBuilds(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, j.Pool, "build-controller") != nil {
		return ErrExecution
	}
	_, e := j.Pool.ExecContext(ctx, `UPDATE build_requests SET state='failed',reason='expired' WHERE state IN ('queued','claimed') AND expires_at<=clock_timestamp()`)
	if e != nil {
		return ErrExecution
	}
	return nil
}
