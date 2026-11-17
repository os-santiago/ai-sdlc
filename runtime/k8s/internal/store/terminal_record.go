package store

import (
	"context"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"time"
)

// TerminalRecord is an authenticated historical fact, never new effect authority.
// Claim tokens remain private to the reader and never enter events or digests.
type TerminalRecord struct {
	Version     string           `json:"version"`
	TerminalID  string           `json:"terminalID"`
	Input       TerminalInput    `json:"input"`
	Progress    TerminalProgress `json:"progress"`
	Source      BuildInput       `json:"source"`
	CompletedAt time.Time        `json:"completedAt"`
}

func (r TerminalRecord) Digest() string { raw, _ := json.Marshal(r); return authority.Digest(raw) }
func readTerminalRecord(ctx context.Context, q Querier, id, role string) (TerminalRecord, error) {
	var r TerminalRecord
	if !terminalID.MatchString(id) || (role != "release-manager" && role != "orchestrator") || AssertComponent(ctx, q, role) != nil {
		return r, ErrExecution
	}
	c := TerminalClaim{ID: id}
	if q.QueryRowContext(ctx, `SELECT claim_token,expires_at,completed_at FROM terminal_requests WHERE id=$1 AND state='completed' AND completed_at<=clock_timestamp()`, id).Scan(&c.Token, &c.Until, &r.CompletedAt) != nil {
		return r, ErrExecution
	}
	var e error
	r.Input, e = terminalArchivedRole(ctx, q, c, role)
	if e != nil {
		return TerminalRecord{}, e
	}
	r.Progress, e = terminalProgress(ctx, q, c, r.Input.Plan)
	if e != nil || r.Progress.NextStep != "" || r.Progress.Uncertain || len(r.Progress.Receipts) != 4 {
		return TerminalRecord{}, ErrExecution
	}
	var closeAt time.Time
	if q.QueryRowContext(ctx, `SELECT recorded_at FROM terminal_receipts WHERE request_id=$1 AND step='close'`, id).Scan(&closeAt) != nil || r.CompletedAt.Before(closeAt) {
		return TerminalRecord{}, ErrExecution
	}
	b := r.Input.Upstream.Upstream.Build.Command
	r.Source, e = readBuildInput(ctx, q, b.MergeID, role)
	if e != nil || r.Source.Source.Repository != b.Repository || r.Source.Source.IssueNumber != b.IssueNumber || r.Source.Source.SourceSHA != b.SourceSHA || r.Source.Source.TreeSHA != b.TreeSHA || r.Source.Source.MergeRecordSHA256 != b.MergeRecordSHA256 || r.Source.Generation != b.Generation || r.Source.IssueRevision != b.IssueRevision {
		return TerminalRecord{}, ErrExecution
	}
	r.Version = "ai-sdlc.terminal-record/v1"
	r.TerminalID = id
	return r, nil
}
func (db *DB) ReadTerminalForProduction(ctx context.Context, id string) (TerminalRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return readTerminalRecord(ctx, db.Pool, id, "release-manager")
}
func (db *DB) ReadProductionForOrchestrator(ctx context.Context, id string) (TerminalRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return readTerminalRecord(ctx, db.Pool, id, "orchestrator")
}
