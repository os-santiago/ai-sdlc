package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
)

// QueueBuildTx is used only inside the authenticated transactional inbox. It
// performs no external effect and reloads installation authority before commit.
func (j *BuildJournal) QueueBuildTx(ctx context.Context, tx *sql.Tx, payload, receipt []byte) error {
	if tx == nil {
		return ErrExecution
	}
	cfg, c, e := j.current(payload, receipt)
	if e != nil {
		return e
	}
	if AssertComponent(ctx, tx, "build-controller") != nil || j.fresh(ctx, tx, cfg, c) != nil {
		return ErrExecution
	}
	inner, _ := json.Marshal(cfg.Inner)
	outer, _ := json.Marshal(cfg.Outer)
	result, e := tx.ExecContext(ctx, `INSERT INTO build_requests(id,merge_id,payload_digest,payload,receipt,inner_policy,outer_policy,expires_at) SELECT $1,$2,$3,$4,$5,$6,$7,$8 WHERE $8>clock_timestamp() AND $8<=clock_timestamp()+interval '1200 seconds'`, c.ID(), c.MergeID, authority.Digest(payload), payload, receipt, inner, outer, time.UnixMilli(c.ExpiresAt))
	if e != nil {
		return ErrExecution
	}
	n, e := result.RowsAffected()
	if e != nil || n != 1 {
		return ErrExecution
	}
	_, _, e = j.current(payload, receipt)
	return e
}

type BuildRequest struct {
	Payload, Receipt []byte
	Command          building.Command
}

// ReadQueuedBuild accepts only the stored, signed request identifier. Source
// inputs and execution choices cannot be supplied by a caller or recovered from
// an existing named Kubernetes workload.
func (j *BuildJournal) ReadQueuedBuild(ctx context.Context, id string) (BuildRequest, error) {
	var out BuildRequest
	if !buildIDPattern.MatchString(id) || AssertComponent(ctx, j.Pool, "build-controller") != nil {
		return out, ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var digest string
	if j.Pool.QueryRowContext(ctx, `SELECT payload,receipt,payload_digest FROM build_requests WHERE id=$1 AND state='queued' AND expires_at>clock_timestamp()`, id).Scan(&out.Payload, &out.Receipt, &digest) != nil || authority.Digest(out.Payload) != digest {
		return BuildRequest{}, ErrExecution
	}
	cfg, c, e := j.current(out.Payload, out.Receipt)
	if e != nil || c.ID() != id {
		return BuildRequest{}, ErrExecution
	}
	if e = j.fresh(ctx, j.Pool, cfg, c); e != nil {
		return BuildRequest{}, e
	}
	out.Command = c
	return out, nil
}
func (j *BuildJournal) NextBuild(ctx context.Context) (string, error) {
	if j == nil || j.DB == nil || j.load == nil {
		return "", ErrExecution
	}
	cfg, e := j.load()
	if e != nil || cfg.Validate() != nil || cfg.Inner.Freeze || cfg.Outer.Freeze {
		return "", ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if AssertComponent(ctx, j.Pool, "build-controller") != nil {
		return "", ErrExecution
	}
	var id string
	budget := int64((buildworkload.BuildBudget + buildworkload.PublishBudget + buildworkload.ObservationMargin) / time.Second)
	e = j.Pool.QueryRowContext(ctx, `SELECT q.id FROM build_requests q
 JOIN merge_requests mr ON mr.id=q.merge_id
 JOIN publication_requests pr ON pr.id=mr.publication_id
 JOIN work_items w ON w.uuid=(convert_from(pr.payload,'UTF8')::jsonb->>'workItemId')::uuid
 JOIN intake_heads h ON h.repository=convert_from(q.payload,'UTF8')::jsonb->>'repository' AND h.issue_number=(convert_from(q.payload,'UTF8')::jsonb->>'issueNumber')::int
 JOIN intake_revisions ir ON ir.repository=h.repository AND ir.issue_number=h.issue_number AND ir.generation=h.generation
 JOIN intake_observations o ON o.repository=h.repository AND o.issue_number=h.issue_number AND o.generation=h.generation AND o.content_key=h.content_key
 JOIN intake_cursors cur ON cur.repository=h.repository AND cur.config_digest=$3
 WHERE q.state='queued' AND w.status='running'
 AND o.config_digest=$3 AND o.authority_revision=$4 AND ir.config_digest=$3
 AND o.observed_at BETWEEN clock_timestamp()-($6*interval '1 second') AND clock_timestamp()+interval '5 seconds'
 AND cur.watermark BETWEEN clock_timestamp()-($6*interval '1 second') AND clock_timestamp()+interval '5 seconds'
 AND h.generation=(convert_from(q.payload,'UTF8')::jsonb->>'generation')::bigint
 AND ir.revision=convert_from(q.payload,'UTF8')::jsonb->>'issueRevision'
 AND convert_from(ir.payload,'UTF8')::jsonb->>'eligible'='true'
 AND q.expires_at>=clock_timestamp()+($1*interval '1 second')
 AND convert_from(q.payload,'UTF8')::jsonb->>'profileSHA256'=$2
 AND convert_from(q.payload,'UTF8')::jsonb->>'intakeConfigSHA256'=$3
 AND convert_from(q.payload,'UTF8')::jsonb->>'innerPolicyRevision'=$4
 AND convert_from(q.payload,'UTF8')::jsonb->>'outerPolicyRevision'=$5
 ORDER BY q.created_at,q.id LIMIT 1`, budget, cfg.ProfileDigest(), cfg.IntakeConfigSHA256, cfg.Inner.Revision(), cfg.Outer.Revision(), cfg.IssueMaxAgeSeconds).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	if e != nil {
		return "", ErrExecution
	}
	return id, nil
}
func (j *BuildJournal) UnusedBuildSlot(ctx context.Context) (building.Slot, error) {
	if j == nil || j.load == nil {
		return building.Slot{}, ErrExecution
	}
	cfg, e := j.load()
	if e != nil || cfg.Validate() != nil || AssertComponent(ctx, j.Pool, "build-controller") != nil {
		return building.Slot{}, ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for _, s := range cfg.Slots {
		var used bool
		if j.Pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM build_slots WHERE slot_id=$1)`, s.ID()).Scan(&used) != nil {
			return building.Slot{}, ErrExecution
		}
		if !used {
			return s, nil
		}
	}
	return building.Slot{}, ErrExecution
}
