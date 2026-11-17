package store

import (
	"context"
	"database/sql"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/deployment"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasecompletion"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"time"
)

type CompletionInput struct {
	Deployment DeploymentRecord
	Upstream   DeploymentInput
}

func readCompletionInput(ctx context.Context, q Querier, id, role string) (CompletionInput, error) {
	var result CompletionInput
	if (role != "deployer" && role != "release-manager" && role != "orchestrator") || !deploymentID.MatchString(id) || AssertComponent(ctx, q, role) != nil {
		return result, ErrExecution
	}
	var claim DeploymentClaim
	claim.ID = id
	if q.QueryRowContext(ctx, `SELECT claim_token,expires_at FROM deployment_requests WHERE id=$1 AND state='deployed'`, id).Scan(&claim.Token, &claim.Until) != nil {
		return result, ErrExecution
	}
	record, e := readDeploymentRecord(ctx, q, claim, role)
	if e != nil {
		return result, e
	}
	upstream, e := readPromotionBundle(ctx, q, record.Command.PromotionID, role)
	if e != nil || record.Validate(upstream.Promotion, upstream.Provenance, upstream.Build) != nil {
		return result, ErrExecution
	}
	result.Deployment, result.Upstream = record, upstream
	return result, nil
}
func (db *DB) ReadDeployedForCompletion(ctx context.Context, id string) (CompletionInput, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return readCompletionInput(ctx, db.Pool, id, "deployer")
}
func (db *DB) ReadDeploymentForRelease(ctx context.Context, id string) (CompletionInput, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return readCompletionInput(ctx, db.Pool, id, "release-manager")
}
func (db *DB) NextDeployedForCompletion(ctx context.Context, cfg releasing.Configuration) (string, error) {
	if cfg.Validate(time.Now()) != nil || AssertComponent(ctx, db.Pool, "deployer") != nil {
		return "", ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, e := db.Pool.QueryContext(ctx, `SELECT d.id FROM deployment_requests d
 JOIN promotion_requests p ON p.id=d.promotion_id
 JOIN provenance_requests pv ON pv.id=p.provenance_id
 JOIN build_requests b ON b.id=pv.build_id
 JOIN merge_requests mr ON mr.id=b.merge_id
 JOIN publication_requests pr ON pr.id=mr.publication_id
 JOIN work_items w ON w.uuid=(convert_from(pr.payload,'UTF8')::jsonb->>'workItemId')::uuid
 JOIN intake_heads h ON h.repository=convert_from(b.payload,'UTF8')::jsonb->>'repository' AND h.issue_number=(convert_from(b.payload,'UTF8')::jsonb->>'issueNumber')::int
 JOIN intake_revisions ir ON ir.repository=h.repository AND ir.issue_number=h.issue_number AND ir.generation=h.generation
 JOIN intake_observations o ON o.repository=h.repository AND o.issue_number=h.issue_number AND o.generation=h.generation AND o.content_key=h.content_key
 JOIN intake_cursors cur ON cur.repository=h.repository AND cur.config_digest=$2
 WHERE d.state='deployed' AND w.status='running' AND convert_from(d.payload,'UTF8')::jsonb->>'configurationSHA256'=$1
 AND h.repository=$5 AND o.config_digest=$2 AND ir.config_digest=$2 AND o.authority_revision=$3
 AND h.generation=(convert_from(b.payload,'UTF8')::jsonb->>'generation')::bigint
 AND ir.revision=convert_from(b.payload,'UTF8')::jsonb->>'issueRevision'
 AND convert_from(ir.payload,'UTF8')::jsonb->>'eligible'='true'
 AND o.observed_at BETWEEN clock_timestamp()-($4*interval '1 second') AND clock_timestamp()+interval '5 seconds'
 AND cur.watermark BETWEEN clock_timestamp()-($4*interval '1 second') AND clock_timestamp()+interval '5 seconds'
 AND NOT EXISTS(SELECT 1 FROM completion_handoffs c WHERE c.deployment_id=d.id)
 ORDER BY d.created_at,d.id LIMIT 32`, deployment.ConfigurationDigest(cfg), cfg.Provenance.Build.IntakeConfigSHA256, cfg.Provenance.Build.Inner.Revision(), cfg.Provenance.Build.IssueMaxAgeSeconds, cfg.Provenance.Build.Outer.Repository)
	if e != nil {
		return "", ErrExecution
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) != nil {
			rows.Close()
			return "", ErrExecution
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return "", ErrExecution
	}
	for _, id := range ids {
		r, e := readCompletionInput(ctx, db.Pool, id, "deployer")
		if e != nil {
			return "", e
		}
		if _, e = releasecompletion.NewCommand(cfg, r.Deployment, r.Upstream.Promotion, r.Upstream.Provenance, r.Upstream.Build, time.Now()); e == nil && freshBuildInput(ctx, db.Pool, cfg.Provenance.Build, r.Upstream.Build.Command, "deployer") == nil {
			return id, nil
		}
	}
	return "", nil
}
func checkCompletionHandoff(ctx context.Context, tx *sql.Tx, load func() (releasing.Configuration, error), signed authority.Receipt, raw []byte) (releasecompletion.Command, error) {
	if tx == nil || load == nil || AssertComponent(ctx, tx, "deployer") != nil {
		return releasecompletion.Command{}, ErrExecution
	}
	cfg, e := load()
	if e != nil {
		return releasecompletion.Command{}, ErrExecution
	}
	c, e := releasecompletion.VerifyCommand(cfg, signed, raw, time.Now())
	if e != nil {
		return c, ErrExecution
	}
	r, e := readCompletionInput(ctx, tx, c.DeploymentID, "deployer")
	if e != nil || c.BindRecords(cfg, r.Deployment, r.Upstream.Promotion, r.Upstream.Provenance, r.Upstream.Build, time.Now()) != nil || freshBuildInput(ctx, tx, cfg.Provenance.Build, r.Upstream.Build.Command, "deployer") != nil {
		return c, ErrExecution
	}
	current, e := load()
	if e != nil || ctx.Err() != nil || c.BindRecords(current, r.Deployment, r.Upstream.Promotion, r.Upstream.Provenance, r.Upstream.Build, time.Now()) != nil {
		return c, ErrExecution
	}
	return c, nil
}
func RecordCompletionHandoff(ctx context.Context, tx *sql.Tx, load func() (releasing.Configuration, error), signed authority.Receipt, raw []byte) error {
	c, e := checkCompletionHandoff(ctx, tx, load, signed, raw)
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO completion_handoffs(deployment_id,command_id,payload_digest)VALUES($1,$2,$3)`, c.DeploymentID, c.ID(), authority.Digest(raw)); e != nil {
		return ErrExecution
	}
	return CheckCompletionHandoff(ctx, tx, load, signed, raw)
}
func CheckCompletionHandoff(ctx context.Context, tx *sql.Tx, load func() (releasing.Configuration, error), signed authority.Receipt, raw []byte) error {
	c, e := checkCompletionHandoff(ctx, tx, load, signed, raw)
	if e != nil {
		return e
	}
	var exists bool
	if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM completion_handoffs WHERE deployment_id=$1 AND command_id=$2 AND payload_digest=$3)`, c.DeploymentID, c.ID(), authority.Digest(raw)).Scan(&exists) != nil || !exists {
		return ErrExecution
	}
	_, e = checkCompletionHandoff(ctx, tx, load, signed, raw)
	return e
}
