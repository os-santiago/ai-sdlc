package store

import (
	"context"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"time"
)

// BuildJournal loads fresh installation authority at each durable fence. No
// HTTP handler or installed controller constructs one in this increment.
type BuildJournal struct {
	*DB
	load func() (building.Configuration, error)
}

func NewBuildJournal(db *DB, load func() (building.Configuration, error)) *BuildJournal {
	return &BuildJournal{db, load}
}
func (j *BuildJournal) current(payload, receipt []byte) (building.Configuration, building.Command, error) {
	var c building.Command
	if j == nil || j.DB == nil || j.load == nil || len(receipt) == 0 || len(receipt) > 4096 {
		return building.Configuration{}, c, ErrExecution
	}
	cfg, e := j.load()
	if e != nil || cfg.Validate() != nil {
		return building.Configuration{}, c, ErrExecution
	}
	var signed authority.Receipt
	if authority.DecodeStrict(receipt, &signed) != nil {
		return building.Configuration{}, c, ErrExecution
	}
	c, e = building.Verify(cfg.Inner, cfg.Outer, signed, payload, time.Now())
	if e != nil || c.ProfileSHA256 != cfg.ProfileDigest() || c.IntakeConfigSHA256 != cfg.IntakeConfigSHA256 {
		return building.Configuration{}, building.Command{}, ErrExecution
	}
	return cfg, c, nil
}
func (j *BuildJournal) fresh(ctx context.Context, q Querier, cfg building.Configuration, c building.Command) error {
	input, e := readBuildInput(ctx, q, c.MergeID, "build-controller")
	s := input.Source
	if e != nil || s.MergeRecordSHA256 != c.MergeRecordSHA256 || s.Repository != c.Repository || s.IssueNumber != c.IssueNumber || s.SourceSHA != c.SourceSHA || s.TreeSHA != c.TreeSHA || input.IssueRevision != c.IssueRevision || input.Generation != c.Generation || input.InnerPolicyRevision != c.InnerPolicyRevision {
		return ErrExecution
	}
	var payload []byte
	key := authority.Digest([]byte(c.IssueRevision + "\x00true\x00" + c.IntakeConfigSHA256 + "\x00" + c.InnerPolicyRevision))
	e = q.QueryRowContext(ctx, `SELECT ir.payload FROM intake_heads h JOIN intake_revisions ir ON ir.repository=h.repository AND ir.issue_number=h.issue_number AND ir.generation=h.generation JOIN intake_observations o ON o.repository=h.repository AND o.issue_number=h.issue_number AND o.generation=h.generation AND o.content_key=h.content_key JOIN intake_cursors cur ON cur.repository=ir.repository AND cur.config_digest=ir.config_digest JOIN work_items w ON w.uuid=$8 WHERE h.repository=$1 AND h.issue_number=$2 AND h.generation=$3 AND h.content_key=$4 AND ir.revision=$5 AND ir.config_digest=$6 AND o.config_digest=$6 AND o.authority_revision=$7 AND w.status='running' AND o.observed_at>=clock_timestamp()-($9*interval '1 second') AND o.observed_at<=clock_timestamp()+interval '5 seconds' AND cur.watermark>=clock_timestamp()-($9*interval '1 second') AND cur.watermark<=clock_timestamp()+interval '5 seconds'`, c.Repository, c.IssueNumber, c.Generation, key, c.IssueRevision, c.IntakeConfigSHA256, c.InnerPolicyRevision, input.WorkItemID, cfg.IssueMaxAgeSeconds).Scan(&payload)
	if e != nil {
		return observationError(e)
	}
	var fields map[string]json.RawMessage
	var observed struct {
		Repository        string
		Revision          string
		Generation        int64
		AuthorityRevision string
		ConfigDigest      string
		Eligible          bool
		Issue             struct {
			Number int
			State  string
		}
	}
	if authority.DecodeStrict(payload, &fields) != nil || json.Unmarshal(payload, &observed) != nil || observed.Repository != c.Repository || observed.Revision != c.IssueRevision || observed.Generation != c.Generation || observed.AuthorityRevision != c.InnerPolicyRevision || observed.ConfigDigest != c.IntakeConfigSHA256 || !observed.Eligible || observed.Issue.Number != c.IssueNumber || observed.Issue.State != "open" {
		return ErrExecution
	}
	return nil
}
