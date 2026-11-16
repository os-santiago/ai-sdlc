package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/deployment"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"time"
)

func readPromotionBundle(ctx context.Context, q Querier, id, role string) (DeploymentInput, error) {
	var r DeploymentInput
	var e error
	r.Promotion, e = readPromotionRecord(ctx, q, id, role)
	if e != nil {
		return r, e
	}
	r.Provenance, e = readProvenanceRecord(ctx, q, r.Promotion.Command.ProvenanceID, role)
	if e != nil {
		return r, e
	}
	r.Build, e = readBuiltRecord(ctx, q, r.Provenance.Command.BuildID, role)
	if e != nil {
		return DeploymentInput{}, e
	}
	return r, nil
}
func (db *DB) ReadPromotionBundle(ctx context.Context, id string) (DeploymentInput, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return readPromotionBundle(ctx, db.Pool, id, "release-manager")
}
func (db *DB) NextPlannedForDeployment(ctx context.Context, cfg releasing.Configuration) (string, error) {
	if cfg.Validate(time.Now()) != nil {
		return "", ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "release-manager") != nil {
		return "", ErrExecution
	}
	var id string
	e := db.Pool.QueryRowContext(ctx, `SELECT q.id FROM promotion_requests q JOIN promotion_records prc ON prc.request_id=q.id
 JOIN provenance_requests pv ON pv.id=q.provenance_id
 JOIN build_requests b ON b.id=pv.build_id
 JOIN merge_requests mr ON mr.id=b.merge_id
 JOIN publication_requests pr ON pr.id=mr.publication_id
 JOIN work_items w ON w.uuid=(convert_from(pr.payload,'UTF8')::jsonb->>'workItemId')::uuid
 JOIN intake_heads h ON h.repository=convert_from(b.payload,'UTF8')::jsonb->>'repository' AND h.issue_number=(convert_from(b.payload,'UTF8')::jsonb->>'issueNumber')::int
 JOIN intake_revisions ir ON ir.repository=h.repository AND ir.issue_number=h.issue_number AND ir.generation=h.generation
 JOIN intake_observations o ON o.repository=h.repository AND o.issue_number=h.issue_number AND o.generation=h.generation AND o.content_key=h.content_key
 JOIN intake_cursors cur ON cur.repository=h.repository AND cur.config_digest=$3
 WHERE q.state='planned' AND b.state='built' AND w.status='running' AND h.repository=$7
 AND (convert_from(prc.plan,'UTF8')::jsonb->>'expiresAt')::bigint >= extract(epoch from clock_timestamp()+interval '14 minutes')*1000
 AND NOT EXISTS(SELECT 1 FROM deployment_handoffs dh WHERE dh.promotion_id=q.id)
 AND convert_from(q.payload,'UTF8')::jsonb->>'targetSHA256'=$8
 AND convert_from(q.payload,'UTF8')::jsonb->>'trustSHA256'=$1
 AND convert_from(q.payload,'UTF8')::jsonb->>'profileSHA256'=$2
 AND convert_from(q.payload,'UTF8')::jsonb->>'intakeConfigSHA256'=$3
 AND convert_from(q.payload,'UTF8')::jsonb->>'innerPolicyRevision'=$4
 AND convert_from(q.payload,'UTF8')::jsonb->>'outerPolicyRevision'=$5
 AND o.config_digest=$3 AND ir.config_digest=$3 AND o.authority_revision=$4
 AND h.generation=(convert_from(b.payload,'UTF8')::jsonb->>'generation')::bigint
 AND ir.revision=convert_from(b.payload,'UTF8')::jsonb->>'issueRevision'
 AND convert_from(ir.payload,'UTF8')::jsonb->>'eligible'='true'
 AND o.observed_at BETWEEN clock_timestamp()-($6*interval '1 second') AND clock_timestamp()+interval '5 seconds'
 AND cur.watermark BETWEEN clock_timestamp()-($6*interval '1 second') AND clock_timestamp()+interval '5 seconds'
 ORDER BY q.created_at,q.id LIMIT 1`, cfg.Provenance.Trust.Digest(), cfg.Provenance.Build.ProfileDigest(), cfg.Provenance.Build.IntakeConfigSHA256, cfg.Provenance.Build.Inner.Revision(), cfg.Provenance.Build.Outer.Revision(), cfg.Provenance.Build.IssueMaxAgeSeconds, cfg.Provenance.Build.Outer.Repository, cfg.Target.Digest()).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	if e != nil {
		return "", ErrExecution
	}
	return id, nil
}
func checkDeploymentHandoff(ctx context.Context, tx *sql.Tx, load func() (releasing.Configuration, error), signed authority.Receipt, raw []byte) (deployment.Command, error) {
	if tx == nil || load == nil || AssertComponent(ctx, tx, "release-manager") != nil {
		return deployment.Command{}, ErrExecution
	}
	input, e := load()
	if e != nil {
		return deployment.Command{}, ErrExecution
	}
	config, e := json.Marshal(input)
	var cfg releasing.Configuration
	if e != nil || len(config) > 327680 || authority.DecodeStrict(config, &cfg) != nil {
		return deployment.Command{}, ErrExecution
	}
	c, e := deployment.VerifyCommand(cfg, signed, raw, time.Now())
	if e != nil {
		return c, ErrExecution
	}
	r, e := readPromotionBundle(ctx, tx, c.PromotionID, "release-manager")
	if e != nil || c.BindRecords(cfg, r.Promotion, r.Provenance, r.Build, time.Now()) != nil || freshBuildInput(ctx, tx, cfg.Provenance.Build, r.Build.Command, "release-manager") != nil {
		return c, ErrExecution
	}
	current, e := load()
	if e != nil || ctx.Err() != nil || c.ExpiresAt-time.Now().UnixMilli() < releasing.MinimumEffectBudget.Milliseconds() || c.BindRecords(current, r.Promotion, r.Provenance, r.Build, time.Now()) != nil {
		return c, ErrExecution
	}
	return c, nil
}
func RecordDeploymentHandoff(ctx context.Context, tx *sql.Tx, load func() (releasing.Configuration, error), signed authority.Receipt, raw []byte) error {
	c, e := checkDeploymentHandoff(ctx, tx, load, signed, raw)
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO deployment_handoffs(promotion_id,command_id,payload_digest)VALUES($1,$2,$3)`, c.PromotionID, c.ID(), authority.Digest(raw)); e != nil {
		return ErrExecution
	}
	return CheckDeploymentHandoff(ctx, tx, load, signed, raw)
}
func CheckDeploymentHandoff(ctx context.Context, tx *sql.Tx, load func() (releasing.Configuration, error), signed authority.Receipt, raw []byte) error {
	c, e := checkDeploymentHandoff(ctx, tx, load, signed, raw)
	if e != nil {
		return e
	}
	var exists bool
	if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_handoffs WHERE promotion_id=$1 AND command_id=$2 AND payload_digest=$3)`, c.PromotionID, c.ID(), authority.Digest(raw)).Scan(&exists) != nil || !exists {
		return ErrExecution
	}
	_, e = checkDeploymentHandoff(ctx, tx, load, signed, raw)
	return e
}
