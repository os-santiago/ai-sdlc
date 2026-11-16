package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
)

// readCleanBuiltRecord uses original immutable observations and cleanup proof,
// never current names or a replacement workload. It does not free the slot.
func readCleanBuiltRecord(ctx context.Context, q Querier, id string) (BuiltRecord, error) {
	r, e := readBuiltRecord(ctx, q, id, "build-controller")
	if e != nil {
		return BuiltRecord{}, e
	}
	var raw []byte
	var hash string
	var at time.Time
	if q.QueryRowContext(ctx, `SELECT proof,digest,recorded_at FROM build_cleanups WHERE request_id=$1 AND recorded_at<=clock_timestamp()`, id).Scan(&raw, &hash, &at) != nil || authority.Digest(raw) != hash {
		return BuiltRecord{}, ErrExecution
	}
	var p BuildCleanupProof
	expected := []CleanupResource{{Kind: "build", Name: "ai-sdlc-build-" + id[len("build-"):len("build-")+32], UID: r.Build.PipelineRunUID}, {Kind: "publish", Name: "ai-sdlc-publish-" + id[len("build-"):len("build-")+32], UID: r.Publish.PipelineRunUID}}
	if authority.DecodeStrict(raw, &p) != nil || p.Version != "ai-sdlc.build-cleanup/v1" || p.BuildID != id || p.SlotDigest != r.Context.Slot.Digest() || p.UnknownCreate || !reflect.DeepEqual(p.Resources, expected) || at.Before(r.RecordedAt) || p.ObservedAbsentAt < r.RecordedAt.UnixMilli() || p.ObservedAbsentAt > at.Add(5*time.Second).UnixMilli() {
		return BuiltRecord{}, ErrExecution
	}
	return r, nil
}
func (db *DB) ReadCleanBuiltRecord(ctx context.Context, id string) (BuiltRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return readCleanBuiltRecord(ctx, db.Pool, id)
}
func (db *DB) NextBuiltForProvenance(ctx context.Context, cfg provenance.Configuration) (string, error) {
	if cfg.Validate(time.Now()) != nil {
		return "", ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "build-controller") != nil {
		return "", ErrExecution
	}
	var id string
	e := db.Pool.QueryRowContext(ctx, `SELECT q.id FROM build_requests q
 JOIN built_records b ON b.request_id=q.id
 JOIN build_contexts retained ON retained.request_id=q.id
 JOIN build_cleanups cl ON cl.request_id=q.id
 JOIN merge_requests mr ON mr.id=q.merge_id
 JOIN publication_requests pr ON pr.id=mr.publication_id
 JOIN work_items w ON w.uuid=(convert_from(pr.payload,'UTF8')::jsonb->>'workItemId')::uuid
 JOIN intake_heads h ON h.repository=convert_from(q.payload,'UTF8')::jsonb->>'repository' AND h.issue_number=(convert_from(q.payload,'UTF8')::jsonb->>'issueNumber')::int
 JOIN intake_revisions ir ON ir.repository=h.repository AND ir.issue_number=h.issue_number AND ir.generation=h.generation
 JOIN intake_observations o ON o.repository=h.repository AND o.issue_number=h.issue_number AND o.generation=h.generation AND o.content_key=h.content_key
 JOIN intake_cursors cur ON cur.repository=h.repository AND cur.config_digest=$2
 WHERE q.state='built' AND w.status='running' AND h.repository=$6
 AND o.config_digest=$2 AND o.authority_revision=$3 AND ir.config_digest=$2
 AND o.observed_at BETWEEN clock_timestamp()-($5*interval '1 second') AND clock_timestamp()+interval '5 seconds'
 AND cur.watermark BETWEEN clock_timestamp()-($5*interval '1 second') AND clock_timestamp()+interval '5 seconds'
 AND h.generation=(convert_from(q.payload,'UTF8')::jsonb->>'generation')::bigint
 AND ir.revision=convert_from(q.payload,'UTF8')::jsonb->>'issueRevision'
 AND convert_from(ir.payload,'UTF8')::jsonb->>'eligible'='true'
 AND convert_from(q.payload,'UTF8')::jsonb->>'profileSHA256'=$1
 AND convert_from(q.payload,'UTF8')::jsonb->>'intakeConfigSHA256'=$2
 AND convert_from(q.payload,'UTF8')::jsonb->>'innerPolicyRevision'=$3
 AND convert_from(q.payload,'UTF8')::jsonb->>'outerPolicyRevision'=$4
 AND NOT EXISTS(SELECT 1 FROM provenance_handoffs ph WHERE ph.build_id=q.id)
 AND NOT EXISTS(SELECT 1 FROM provenance_handoffs WHERE created_at>clock_timestamp()-interval '125 seconds')
 ORDER BY b.recorded_at,q.id LIMIT 1`, cfg.Build.ProfileDigest(), cfg.Build.IntakeConfigSHA256, cfg.Build.Inner.Revision(), cfg.Build.Outer.Revision(), cfg.Build.IssueMaxAgeSeconds, cfg.Build.Outer.Repository).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	if e != nil {
		return "", ErrExecution
	}
	return id, nil
}

// RecordProvenanceHandoff commits with the build-controller's signed outbox.
// Reloading after SQL waits prevents a stale configuration from surviving them.
func RecordProvenanceHandoff(ctx context.Context, tx *sql.Tx, load func() (provenance.Configuration, error), signed authority.Receipt, payload []byte) error {
	if tx == nil || load == nil || AssertComponent(ctx, tx, "build-controller") != nil {
		return ErrExecution
	}
	current := func() (provenance.Configuration, provenance.Command, error) {
		var cfg provenance.Configuration
		var c provenance.Command
		in, e := load()
		if e != nil {
			return cfg, c, ErrExecution
		}
		raw, e := json.Marshal(in)
		if e != nil || len(raw) > 196608 || authority.DecodeStrict(raw, &cfg) != nil || ctx.Err() != nil {
			return cfg, c, ErrExecution
		}
		c, e = provenance.VerifyCommand(cfg, signed, payload, time.Now())
		if e != nil {
			return cfg, c, ErrExecution
		}
		return cfg, c, nil
	}
	if busy, e := provenanceWindowBusy(ctx, tx); e != nil || busy {
		return ErrExecution
	}

	cfg, c, e := current()
	if e != nil {
		return e
	}
	r, e := readCleanBuiltRecord(ctx, tx, c.BuildID)
	if e != nil || c.BindRecord(cfg, r, time.Now()) != nil || freshBuildInput(ctx, tx, cfg.Build, r.Command, "build-controller") != nil {
		return ErrExecution
	}
	if _, _, e = current(); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO provenance_handoffs(build_id,command_id,payload_digest) VALUES($1,$2,$3)`, c.BuildID, c.ID(), authority.Digest(payload)); e != nil {
		return ErrExecution
	}
	return CheckProvenanceHandoff(ctx, tx, load, signed, payload)
}

// CheckProvenanceHandoff is repeated after outbox I/O in the same transaction.
func CheckProvenanceHandoff(ctx context.Context, tx *sql.Tx, load func() (provenance.Configuration, error), signed authority.Receipt, payload []byte) error {
	if tx == nil || load == nil || AssertComponent(ctx, tx, "build-controller") != nil {
		return ErrExecution
	}
	input, e := load()
	if e != nil {
		return ErrExecution
	}
	raw, e := json.Marshal(input)
	var cfg provenance.Configuration
	if e != nil || len(raw) > 196608 || authority.DecodeStrict(raw, &cfg) != nil {
		return ErrExecution
	}
	c, e := provenance.VerifyCommand(cfg, signed, payload, time.Now())
	if e != nil {
		return ErrExecution
	}
	var exists bool
	if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM provenance_handoffs WHERE build_id=$1 AND command_id=$2 AND payload_digest=$3)`, c.BuildID, c.ID(), authority.Digest(payload)).Scan(&exists) != nil || !exists {
		return ErrExecution
	}
	r, e := readCleanBuiltRecord(ctx, tx, c.BuildID)
	if e != nil || c.BindRecord(cfg, r, time.Now()) != nil || freshBuildInput(ctx, tx, cfg.Build, r.Command, "build-controller") != nil {
		return ErrExecution
	}
	cfg, e = load()
	if e != nil || ctx.Err() != nil {
		return ErrExecution
	}
	_, e = provenance.VerifyCommand(cfg, signed, payload, time.Now())
	if e != nil {
		return ErrExecution
	}
	return nil
}

// The signed command is bounded to two minutes with at most five seconds of
// future clock skew. Holding its reservation for 125 seconds from insertion
// therefore covers its entire original lifetime; delivery flags cannot release
// it early. This lock is distinct from the session-level controller lease.
func provenanceWindowBusy(ctx context.Context, tx *sql.Tx) (bool, error) {
	if _, e := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(73421036)`); e != nil {
		return true, ErrExecution
	}
	var busy bool
	if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM provenance_handoffs WHERE created_at>clock_timestamp()-interval '125 seconds')`).Scan(&busy) != nil {
		return true, ErrExecution
	}
	return busy, nil
}
