package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// NextProvenance selects a queued request, never a caller-provided artifact.
// Per-row claims arbitrate replicas; claimed attempts cannot be selected again.
func (j *ProvenanceJournal) NextProvenance(ctx context.Context) (string, error) {
	cfg, e := j.configuration()
	if e != nil || cfg.Validate(time.Now()) != nil {
		return "", ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, j.Pool, "verifier") != nil {
		return "", ErrExecution
	}
	var id string
	e = j.Pool.QueryRowContext(ctx, `SELECT q.id FROM provenance_requests q
 JOIN build_requests b ON b.id=q.build_id
 JOIN merge_requests mr ON mr.id=b.merge_id
 JOIN publication_requests pr ON pr.id=mr.publication_id
 JOIN work_items w ON w.uuid=(convert_from(pr.payload,'UTF8')::jsonb->>'workItemId')::uuid
 JOIN intake_heads h ON h.repository=convert_from(b.payload,'UTF8')::jsonb->>'repository' AND h.issue_number=(convert_from(b.payload,'UTF8')::jsonb->>'issueNumber')::int
 JOIN intake_revisions ir ON ir.repository=h.repository AND ir.issue_number=h.issue_number AND ir.generation=h.generation
 JOIN intake_observations o ON o.repository=h.repository AND o.issue_number=h.issue_number AND o.generation=h.generation AND o.content_key=h.content_key
 JOIN intake_cursors cur ON cur.repository=h.repository AND cur.config_digest=$3
 WHERE q.state='queued' AND b.state='built' AND w.status='running' AND h.repository=$7
 AND q.expires_at>clock_timestamp()+interval '65 seconds'
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
 ORDER BY q.created_at,q.id LIMIT 1`, cfg.Trust.Digest(), cfg.Build.ProfileDigest(), cfg.Build.IntakeConfigSHA256, cfg.Build.Inner.Revision(), cfg.Build.Outer.Revision(), cfg.Build.IssueMaxAgeSeconds, cfg.Build.Outer.Repository).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	if e != nil {
		return "", ErrExecution
	}
	return id, nil
}
