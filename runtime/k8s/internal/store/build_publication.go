package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
)

func (j *BuildJournal) buildContext(ctx context.Context, q Querier, c BuildClaim) (building.Command, building.Slot, error) {
	var payload, receipt, raw []byte
	var slot building.Slot
	if q.QueryRowContext(ctx, `SELECT r.payload,r.receipt,s.slot FROM build_requests r JOIN build_slots s ON s.request_id=r.id WHERE r.id=$1 AND r.claim_token=$2 AND s.slot_id=$3 AND s.slot_digest=$4`, c.ID, c.Token, c.SlotID, c.SlotDigest).Scan(&payload, &receipt, &raw) != nil {
		return building.Command{}, slot, ErrExecution
	}
	_, cmd, e := j.current(payload, receipt)
	if e != nil || authority.DecodeStrict(raw, &slot) != nil || slot.Validate() != nil || slot.ID() != c.SlotID || slot.Digest() != c.SlotDigest {
		return building.Command{}, building.Slot{}, ErrExecution
	}
	return cmd, slot, nil
}
func readBuildPlan(ctx context.Context, q Querier, id string) (building.Plan, error) {
	var p building.Plan
	var digest string
	var raw []byte
	if q.QueryRowContext(ctx, `SELECT digest,plan FROM build_plans WHERE request_id=$1`, id).Scan(&digest, &raw) != nil || authority.DecodeStrict(raw, &p) != nil || p.Digest() != digest {
		return p, ErrExecution
	}
	return p, nil
}
func readBuildObservation(ctx context.Context, q Querier, id, kind string) (building.Termination, time.Time, error) {
	var o building.Termination
	var raw []byte
	var digest string
	var at time.Time
	if q.QueryRowContext(ctx, `SELECT digest,observation,recorded_at FROM build_observations WHERE request_id=$1 AND kind=$2`, id, kind).Scan(&digest, &raw, &at) != nil || authority.DecodeStrict(raw, &o) != nil || o.Digest() != digest || o.Phase != kind {
		return o, at, ErrExecution
	}
	return o, at, nil
}
func observedEffect(ctx context.Context, q Querier, id string, o building.Termination) error {
	var uid, digest string
	if q.QueryRowContext(ctx, `SELECT resource_uid,request_digest FROM build_effects WHERE request_id=$1 AND kind=$2 AND resource_uid IS NOT NULL`, id, o.Phase).Scan(&uid, &digest) != nil || uid != o.PipelineRunUID || digest != o.RequestSHA256 {
		return ErrExecution
	}
	return nil
}

// RecordBuildTermination accepts only a fresh authenticated observation from
// the owning transport, after it checks complete admitted Pod spec/status.
func (j *BuildJournal) RecordBuildTermination(ctx context.Context, c BuildClaim, o building.Termination) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if j.lock(ctx, tx, c) != nil {
			return ErrExecution
		}
		cmd, slot, e := j.buildContext(ctx, tx, c)
		if e != nil {
			return ErrExecution
		}
		p, e := readBuildPlan(ctx, tx, c.ID)
		if e != nil || o.Validate(cmd, p, slot, time.Now()) != nil || observedEffect(ctx, tx, c.ID, o) != nil {
			return ErrExecution
		}
		if o.Phase == "publish" {
			b, at, e := readBuildObservation(ctx, tx, c.ID, "build")
			if e != nil || b.Validate(cmd, p, slot, at) != nil || !orderedTerminations(b, o) {
				return ErrExecution
			}
		}
		raw, _ := json.Marshal(o)
		if _, e = tx.ExecContext(ctx, `INSERT INTO build_observations(request_id,kind,digest,observation) VALUES($1,$2,$3,$4)`, c.ID, o.Phase, o.Digest(), raw); e != nil {
			return ErrExecution
		}
		return j.check(ctx, tx, c)
	})
}
func orderedTerminations(b, p building.Termination) bool {
	if b.PipelineRunUID == p.PipelineRunUID || b.TaskRunUID == p.TaskRunUID || b.PodUID == p.PodUID || b.ObservedAt > p.ObservedAt {
		return false
	}
	objectIDs := map[string]bool{b.PipelineRunUID: true, b.TaskRunUID: true, b.PodUID: true}
	for _, id := range []string{p.PipelineRunUID, p.TaskRunUID, p.PodUID} {
		if objectIDs[id] {
			return false
		}
	}
	var finished int64
	ids := map[string]bool{}
	for _, c := range b.Containers {
		ids[c.ContainerID] = true
		if c.FinishedAt > finished {
			finished = c.FinishedAt
		}
	}
	for _, c := range p.Containers {
		if c.StartedAt < finished || ids[c.ContainerID] {
			return false
		}
	}
	return true
}
func (j *BuildJournal) BeginPublishEffect(ctx context.Context, c BuildClaim) (BuildEffect, error) {
	if !buildIDPattern.MatchString(c.ID) {
		return BuildEffect{}, ErrExecution
	}
	effect := BuildEffect{Claim: c, Token: NewID(), Name: "ai-sdlc-publish-" + c.ID[len("build-"):len("build-")+32], Kind: "publish"}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if j.lock(ctx, tx, c) != nil {
			return ErrExecution
		}
		cmd, slot, e := j.buildContext(ctx, tx, c)
		if e != nil {
			return ErrExecution
		}
		p, e := readBuildPlan(ctx, tx, c.ID)
		if e != nil || p.Validate(cmd, slot) != nil {
			return ErrExecution
		}
		b, at, e := readBuildObservation(ctx, tx, c.ID, "build")
		if e != nil || b.Validate(cmd, p, slot, at) != nil || observedEffect(ctx, tx, c.ID, b) != nil {
			return ErrExecution
		}
		effect.RequestDigest = p.PublishRequestSHA256
		if _, e = tx.ExecContext(ctx, `INSERT INTO build_effects(request_id,kind,token,request_digest,resource_name) VALUES($1,'publish',$2,$3,$4)`, c.ID, effect.Token, effect.RequestDigest, effect.Name); e != nil {
			return ErrExecution
		}
		return j.check(ctx, tx, c)
	})
	if e != nil {
		return BuildEffect{}, ErrExecution
	}
	return effect, nil
}
func (j *BuildJournal) FinishBuild(ctx context.Context, c BuildClaim, r building.Result) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if j.lock(ctx, tx, c) != nil {
			return ErrExecution
		}
		cmd, slot, e := j.buildContext(ctx, tx, c)
		if e != nil {
			return ErrExecution
		}
		p, e := readBuildPlan(ctx, tx, c.ID)
		if e != nil || p.Validate(cmd, slot) != nil {
			return ErrExecution
		}
		b, bt, e := readBuildObservation(ctx, tx, c.ID, "build")
		if e != nil || b.Validate(cmd, p, slot, bt) != nil || observedEffect(ctx, tx, c.ID, b) != nil {
			return ErrExecution
		}
		pub, pt, e := readBuildObservation(ctx, tx, c.ID, "publish")
		if e != nil || pub.Validate(cmd, p, slot, pt) != nil || observedEffect(ctx, tx, c.ID, pub) != nil || !orderedTerminations(b, pub) || r.Validate(cmd, p, b, pub) != nil {
			return ErrExecution
		}
		raw, _ := json.Marshal(r)
		if _, e = tx.ExecContext(ctx, `INSERT INTO built_records(request_id,digest,result) VALUES($1,$2,$3)`, c.ID, authority.Digest(raw), raw); e != nil {
			return ErrExecution
		}
		if j.check(ctx, tx, c) != nil {
			return ErrExecution
		}
		result, e := tx.ExecContext(ctx, `UPDATE build_requests SET state='built' WHERE id=$1 AND claim_token=$2 AND state='claimed' AND expires_at>clock_timestamp()`, c.ID, c.Token)
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
