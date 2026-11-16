package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
)

// CleanupResource is an original acknowledged UID, never a name to adopt.
type CleanupResource struct{ Kind, Name, UID string }
type BuildCleanup struct {
	Claim         BuildClaim
	Slot          building.Slot
	Resources     []CleanupResource
	UnknownCreate bool
}

// CleanupProof is supplied only by the trusted UID-bound cleanup transport.
// Unknown creates remain quarantined and no outcome ever frees physical storage.
type BuildCleanupProof struct {
	Version          string            `json:"version"`
	BuildID          string            `json:"buildID"`
	SlotDigest       string            `json:"slotDigest"`
	Resources        []CleanupResource `json:"resources"`
	UnknownCreate    bool              `json:"unknownCreate"`
	ObservedAbsentAt int64             `json:"observedAbsentAt"`
}

// ReadBuildCleanup intentionally does not load current policy or eligibility.
// It accepts only terminal requests. Credential removal requires installer-owned
// cleanup first; a policy freeze alone does not remove the installed DB identity.
func (j *BuildJournal) ReadBuildCleanup(ctx context.Context, c BuildClaim) (BuildCleanup, error) {
	out := BuildCleanup{Claim: c}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if !buildIDPattern.MatchString(c.ID) || !lifecycleUUID.MatchString(c.Token) || AssertComponent(ctx, j.Pool, "build-controller") != nil {
		return out, ErrExecution
	}
	var raw, payload, receipt, inner, outer []byte
	if j.Pool.QueryRowContext(ctx, `SELECT s.slot,r.payload,r.receipt,r.inner_policy,r.outer_policy FROM build_requests r JOIN build_slots s ON s.request_id=r.id WHERE r.id=$1 AND r.claim_token=$2 AND s.slot_id=$3 AND s.slot_digest=$4 AND r.state IN ('failed','built')`, c.ID, c.Token, c.SlotID, c.SlotDigest).Scan(&raw, &payload, &receipt, &inner, &outer) != nil {
		return BuildCleanup{}, ErrExecution
	}
	cmd, e := archivedBuild(payload, receipt, inner, outer)
	if e != nil || cmd.ID() != c.ID || authority.DecodeStrict(raw, &out.Slot) != nil || out.Slot.Validate() != nil || out.Slot.ID() != c.SlotID || out.Slot.Digest() != c.SlotDigest {
		return BuildCleanup{}, ErrExecution
	}
	rows, e := j.Pool.QueryContext(ctx, `SELECT kind,resource_name,coalesce(resource_uid::text,'') FROM build_effects WHERE request_id=$1 ORDER BY kind`, c.ID)
	if e != nil {
		return BuildCleanup{}, ErrExecution
	}
	defer rows.Close()
	for rows.Next() {
		var r CleanupResource
		if rows.Scan(&r.Kind, &r.Name, &r.UID) != nil || (r.Kind != "build" && r.Kind != "publish") || r.Name != "ai-sdlc-"+r.Kind+"-"+c.ID[len("build-"):len("build-")+32] {
			return BuildCleanup{}, ErrExecution
		}
		if r.UID == "" {
			out.UnknownCreate = true
			continue
		}
		if !lifecycleUUID.MatchString(r.UID) {
			return BuildCleanup{}, ErrExecution
		}
		out.Resources = append(out.Resources, r)
	}
	if rows.Err() != nil {
		return BuildCleanup{}, ErrExecution
	}
	return out, nil
}

// NextBuildRecovery atomically terminates an interrupted claim before returning
// its original identity. It includes completed builds and skips durable cleanup
// acknowledgements. No returned value can authorize a new execution effect.
func (j *BuildJournal) NextBuildRecovery(ctx context.Context) (BuildClaim, error) {
	var c BuildClaim
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	e := j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "build-controller") != nil {
			return ErrExecution
		}
		var state string
		e := tx.QueryRowContext(ctx, `SELECT r.id,r.claim_token,r.claim_until,s.slot_id,s.slot_digest,r.state FROM build_requests r JOIN build_slots s ON s.request_id=r.id WHERE r.claim_token IS NOT NULL AND r.state IN ('claimed','failed','built') AND NOT EXISTS(SELECT 1 FROM build_cleanups c WHERE c.request_id=r.id) ORDER BY r.created_at,r.id LIMIT 1 FOR UPDATE OF r`).Scan(&c.ID, &c.Token, &c.Until, &c.SlotID, &c.SlotDigest, &state)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return ErrExecution
		}
		if state == "claimed" {
			r, e := tx.ExecContext(ctx, `UPDATE build_requests SET state='failed',reason='uncertain' WHERE id=$1 AND claim_token=$2 AND state='claimed'`, c.ID, c.Token)
			if e != nil {
				return ErrExecution
			}
			n, e := r.RowsAffected()
			if e != nil || n != 1 {
				return ErrExecution
			}
		}
		return nil
	})
	if e != nil {
		return BuildClaim{}, ErrExecution
	}
	return c, nil
}

func (j *BuildJournal) RecordBuildCleanup(ctx context.Context, c BuildClaim, p BuildCleanupProof) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	observed := time.UnixMilli(p.ObservedAbsentAt)
	now := time.Now()
	if p.Version != "ai-sdlc.build-cleanup/v1" || p.BuildID != c.ID || p.SlotDigest != c.SlotDigest || observed.Before(now.Add(-30*time.Second)) || observed.After(now.Add(5*time.Second)) {
		return ErrExecution
	}
	original, e := j.ReadBuildCleanup(ctx, c)
	if e != nil || p.UnknownCreate != original.UnknownCreate || !reflect.DeepEqual(p.Resources, original.Resources) {
		return ErrExecution
	}
	raw, _ := json.Marshal(p)
	return j.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "build-controller") != nil {
			return ErrExecution
		}
		var id string
		if tx.QueryRowContext(ctx, `SELECT id FROM build_requests WHERE id=$1 AND claim_token=$2 AND state IN ('failed','built') FOR UPDATE`, c.ID, c.Token).Scan(&id) != nil {
			return ErrExecution
		}
		_, e := tx.ExecContext(ctx, `INSERT INTO build_cleanups(request_id,digest,proof) VALUES($1,$2,$3)`, c.ID, authority.Digest(raw), raw)
		if e != nil {
			return ErrExecution
		}
		return nil
	})
}
