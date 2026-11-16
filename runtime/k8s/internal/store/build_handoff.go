package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
)

// NextMergedBuild selects current eligible history, not execution authority.
func (db *DB) NextMergedBuild(ctx context.Context, cfg building.Configuration) (string, error) {
	if cfg.Validate() != nil || cfg.Inner.Freeze || cfg.Outer.Freeze {
		return "", ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, db.Pool, "release-manager") != nil {
		return "", ErrExecution
	}
	var id string
	e := db.Pool.QueryRowContext(ctx, `SELECT m.merge_id FROM merged_records m
 JOIN merge_requests mr ON mr.id=m.merge_id
 JOIN publication_requests pr ON pr.id=mr.publication_id
 JOIN work_items w ON w.uuid=(convert_from(pr.payload,'UTF8')::jsonb->>'workItemId')::uuid
 JOIN intake_heads h ON h.repository=convert_from(pr.payload,'UTF8')::jsonb->>'repository' AND h.issue_number=(convert_from(pr.payload,'UTF8')::jsonb->>'issueNumber')::int
 JOIN intake_revisions ir ON ir.repository=h.repository AND ir.issue_number=h.issue_number AND ir.generation=h.generation
 JOIN intake_observations o ON o.repository=h.repository AND o.issue_number=h.issue_number AND o.generation=h.generation AND o.content_key=h.content_key
 JOIN intake_cursors cur ON cur.repository=h.repository AND cur.config_digest=$2
 WHERE w.status='running' AND mr.authority_revision=$1 AND h.repository=$3
 AND ir.config_digest=$2 AND o.config_digest=$2 AND o.authority_revision=$1
 AND h.generation=(convert_from(pr.payload,'UTF8')::jsonb->>'generation')::bigint
 AND ir.revision=convert_from(pr.payload,'UTF8')::jsonb->>'issueRevision'
 AND convert_from(ir.payload,'UTF8')::jsonb->>'eligible'='true'
 AND o.observed_at BETWEEN clock_timestamp()-($4*interval '1 second') AND clock_timestamp()+interval '5 seconds'
 AND cur.watermark BETWEEN clock_timestamp()-($4*interval '1 second') AND clock_timestamp()+interval '5 seconds'
 AND NOT EXISTS(SELECT 1 FROM build_handoffs h WHERE h.merge_id=m.merge_id)
 ORDER BY m.merged_at,m.merge_id LIMIT 1`, cfg.Inner.Revision(), cfg.IntakeConfigSHA256, cfg.Inner.Repository, cfg.IssueMaxAgeSeconds).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	if e != nil {
		return "", ErrExecution
	}
	return id, nil
}

// RecordBuildHandoff is atomic with delivery.Append in the owning component.
// Every fact and current issue fence is re-read inside that transaction.
func RecordBuildHandoff(ctx context.Context, tx *sql.Tx, cfg building.Configuration, signed authority.Receipt, payload []byte) error {
	if tx == nil || cfg.Validate() != nil || AssertComponent(ctx, tx, "release-manager") != nil {
		return ErrExecution
	}
	c, e := building.Verify(cfg.Inner, cfg.Outer, signed, payload, time.Now())
	if e != nil || c.ProfileSHA256 != cfg.ProfileDigest() || c.IntakeConfigSHA256 != cfg.IntakeConfigSHA256 || freshBuildInput(ctx, tx, cfg, c, "release-manager") != nil {
		return ErrExecution
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO build_handoffs(merge_id,command_id,payload_digest) VALUES($1,$2,$3)`, c.MergeID, c.ID(), authority.Digest(payload))
	if e != nil {
		return ErrExecution
	}
	return nil
}
