package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"time"
)

func (db *DB) NextVerifiedForPromotion(ctx context.Context, cfg releasing.Configuration) (string, error) {
	if cfg.Validate(time.Now()) != nil {
		return "", ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "verifier") != nil {
		return "", ErrExecution
	}
	var id string
	e := db.Pool.QueryRowContext(ctx, `SELECT p.id FROM provenance_requests p
 JOIN provenance_records r ON r.request_id=p.id
 JOIN build_requests q ON q.id=p.build_id
 JOIN merge_requests mr ON mr.id=q.merge_id
 JOIN publication_requests pr ON pr.id=mr.publication_id
 JOIN work_items w ON w.uuid=(convert_from(pr.payload,'UTF8')::jsonb->>'workItemId')::uuid
 JOIN intake_heads h ON h.repository=convert_from(q.payload,'UTF8')::jsonb->>'repository' AND h.issue_number=(convert_from(q.payload,'UTF8')::jsonb->>'issueNumber')::int
 JOIN intake_revisions ir ON ir.repository=h.repository AND ir.issue_number=h.issue_number AND ir.generation=h.generation
 JOIN intake_observations o ON o.repository=h.repository AND o.issue_number=h.issue_number AND o.generation=h.generation AND o.content_key=h.content_key
 JOIN intake_cursors cur ON cur.repository=h.repository AND cur.config_digest=$3
 WHERE p.state='verified' AND q.state='built' AND w.status='running' AND h.repository=$7
 AND convert_from(p.payload,'UTF8')::jsonb->>'trustSHA256'=$1
 AND convert_from(p.payload,'UTF8')::jsonb->>'profileSHA256'=$2
 AND convert_from(p.payload,'UTF8')::jsonb->>'intakeConfigSHA256'=$3
 AND convert_from(p.payload,'UTF8')::jsonb->>'innerPolicyRevision'=$4
 AND convert_from(p.payload,'UTF8')::jsonb->>'outerPolicyRevision'=$5
 AND o.config_digest=$3 AND ir.config_digest=$3 AND o.authority_revision=$4
 AND h.generation=(convert_from(q.payload,'UTF8')::jsonb->>'generation')::bigint
 AND ir.revision=convert_from(q.payload,'UTF8')::jsonb->>'issueRevision'
 AND convert_from(ir.payload,'UTF8')::jsonb->>'eligible'='true'
 AND o.observed_at BETWEEN clock_timestamp()-($6*interval '1 second') AND clock_timestamp()+interval '5 seconds'
 AND cur.watermark BETWEEN clock_timestamp()-($6*interval '1 second') AND clock_timestamp()+interval '5 seconds'
 AND NOT EXISTS(SELECT 1 FROM promotion_handoffs ph WHERE ph.provenance_id=p.id)
 ORDER BY r.recorded_at,p.id LIMIT 1`, cfg.Provenance.Trust.Digest(), cfg.Provenance.Build.ProfileDigest(), cfg.Provenance.Build.IntakeConfigSHA256, cfg.Provenance.Build.Inner.Revision(), cfg.Provenance.Build.Outer.Revision(), cfg.Provenance.Build.IssueMaxAgeSeconds, cfg.Target.Repository).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	if e != nil {
		return "", ErrExecution
	}
	return id, nil
}
func checkPromotionHandoff(ctx context.Context, tx *sql.Tx, load func() (releasing.Configuration, error), signed authority.Receipt, payload []byte) (releasing.Command, error) {
	if tx == nil || load == nil || AssertComponent(ctx, tx, "verifier") != nil {
		return releasing.Command{}, ErrExecution
	}
	input, e := load()
	if e != nil {
		return releasing.Command{}, ErrExecution
	}
	raw, e := json.Marshal(input)
	var cfg releasing.Configuration
	if e != nil || len(raw) > 327680 || authority.DecodeStrict(raw, &cfg) != nil {
		return releasing.Command{}, ErrExecution
	}
	c, e := releasing.VerifyCommand(cfg, signed, payload, time.Now())
	if e != nil {
		return c, ErrExecution
	}
	p, e := readProvenanceRecord(ctx, tx, c.ProvenanceID, "verifier")
	if e != nil {
		return c, ErrExecution
	}
	b, e := readBuiltRecord(ctx, tx, p.Command.BuildID, "verifier")
	if e != nil || c.BindRecords(cfg, p, b, time.Now()) != nil || freshBuildInput(ctx, tx, cfg.Provenance.Build, b.Command, "verifier") != nil {
		return c, ErrExecution
	}
	current, e := load()
	if e != nil || ctx.Err() != nil || c.BindRecords(current, p, b, time.Now()) != nil {
		return c, ErrExecution
	}
	return c, nil
}
func RecordPromotionHandoff(ctx context.Context, tx *sql.Tx, load func() (releasing.Configuration, error), signed authority.Receipt, payload []byte) error {
	c, e := checkPromotionHandoff(ctx, tx, load, signed, payload)
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO promotion_handoffs(provenance_id,command_id,payload_digest)VALUES($1,$2,$3)`, c.ProvenanceID, c.ID(), authority.Digest(payload)); e != nil {
		return ErrExecution
	}
	return CheckPromotionHandoff(ctx, tx, load, signed, payload)
}
func CheckPromotionHandoff(ctx context.Context, tx *sql.Tx, load func() (releasing.Configuration, error), signed authority.Receipt, payload []byte) error {
	c, e := checkPromotionHandoff(ctx, tx, load, signed, payload)
	if e != nil {
		return e
	}
	var exists bool
	if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM promotion_handoffs WHERE provenance_id=$1 AND command_id=$2 AND payload_digest=$3)`, c.ProvenanceID, c.ID(), authority.Digest(payload)).Scan(&exists) != nil || !exists {
		return ErrExecution
	}
	// Repeat after the final handoff read, including any database wait.
	_, e = checkPromotionHandoff(ctx, tx, load, signed, payload)
	return e
}
