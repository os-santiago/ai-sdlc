package store

import (
	"context"
	"database/sql"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/deployment"
	"time"
)

func (j *DeploymentJournal) NextDeployment(ctx context.Context) (string, error) {
	cfg, e := j.configuration()
	if e != nil || cfg.Validate(time.Now()) != nil {
		return "", ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, j.Pool, "deployer") != nil {
		return "", ErrExecution
	}
	var id string
	e = j.Pool.QueryRowContext(ctx, `SELECT d.id FROM deployment_requests d JOIN promotion_requests q ON q.id=d.promotion_id JOIN promotion_records prc ON prc.request_id=q.id
 JOIN provenance_requests pv ON pv.id=q.provenance_id
 JOIN build_requests b ON b.id=pv.build_id
 JOIN merge_requests mr ON mr.id=b.merge_id
 JOIN publication_requests pr ON pr.id=mr.publication_id
 JOIN work_items w ON w.uuid=(convert_from(pr.payload,'UTF8')::jsonb->>'workItemId')::uuid
 JOIN intake_heads h ON h.repository=convert_from(b.payload,'UTF8')::jsonb->>'repository' AND h.issue_number=(convert_from(b.payload,'UTF8')::jsonb->>'issueNumber')::int
 JOIN intake_revisions ir ON ir.repository=h.repository AND ir.issue_number=h.issue_number AND ir.generation=h.generation
 JOIN intake_observations o ON o.repository=h.repository AND o.issue_number=h.issue_number AND o.generation=h.generation AND o.content_key=h.content_key
 JOIN intake_cursors cur ON cur.repository=h.repository AND cur.config_digest=$3
 WHERE d.state='queued' AND q.state='planned' AND b.state='built' AND w.status='running' AND h.repository=$7
 AND (convert_from(prc.plan,'UTF8')::jsonb->>'expiresAt')::bigint >= extract(epoch from clock_timestamp()+interval '14 minutes')*1000
 AND d.expires_at>=clock_timestamp()+interval '14 minutes'
 AND convert_from(d.payload,'UTF8')::jsonb->>'configurationSHA256'=$9
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
 ORDER BY q.created_at,q.id LIMIT 1`, cfg.Provenance.Trust.Digest(), cfg.Provenance.Build.ProfileDigest(), cfg.Provenance.Build.IntakeConfigSHA256, cfg.Provenance.Build.Inner.Revision(), cfg.Provenance.Build.Outer.Revision(), cfg.Provenance.Build.IssueMaxAgeSeconds, cfg.Provenance.Build.Outer.Repository, cfg.Target.Digest(), deployment.ConfigurationDigest(cfg)).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	if e != nil {
		return "", ErrExecution
	}
	return id, nil
}
