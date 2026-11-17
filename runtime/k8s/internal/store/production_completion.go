package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/productioncompletion"
)

func ProductionFact(r TerminalRecord) productioncompletion.Fact {
	return productioncompletion.Fact{TerminalID: r.TerminalID, TerminalRecordSHA256: r.Digest(), WorkItemID: r.Source.WorkItemID, Repository: r.Source.Source.Repository, IssueNumber: r.Source.Source.IssueNumber, Generation: r.Source.Generation, IssueRevision: r.Source.IssueRevision, SourceSHA: r.Source.Source.SourceSHA, Image: r.Input.Plan.Image, ReleaseURL: r.Progress.Receipts[3].ReleaseURL, CompletedAt: r.CompletedAt.UnixMilli()}
}
func (db *DB) NextTerminalForProduction(ctx context.Context) (string, error) {
	if AssertComponent(ctx, db.Pool, "release-manager") != nil {
		return "", ErrExecution
	}
	var id string
	e := db.Pool.QueryRowContext(ctx, `SELECT q.id FROM terminal_requests q WHERE q.state='completed' AND NOT EXISTS(SELECT 1 FROM production_handoffs h WHERE h.terminal_id=q.id) ORDER BY q.completed_at,q.id LIMIT 1`).Scan(&id)
	if e == sql.ErrNoRows {
		return "", nil
	}
	if e != nil {
		return "", ErrExecution
	}
	return id, nil
}
func CheckProductionHandoff(ctx context.Context, tx *sql.Tx, load func() (authority.Policy, error), signed authority.Receipt, raw []byte) error {
	if tx == nil || load == nil || AssertComponent(ctx, tx, "release-manager") != nil {
		return ErrExecution
	}
	p, e := load()
	if e != nil {
		return ErrExecution
	}
	c, e := productioncompletion.Verify(p, signed, raw)
	if e != nil {
		return ErrExecution
	}
	r, e := readTerminalRecord(ctx, tx, c.Fact.TerminalID, "release-manager")
	if e != nil || ProductionFact(r) != c.Fact {
		return ErrExecution
	}
	p, e = load()
	if e != nil || ctx.Err() != nil || c.Validate(p, r.CompletedAt) != nil {
		return ErrExecution
	}
	return nil
}
func RecordProductionHandoff(ctx context.Context, tx *sql.Tx, load func() (authority.Policy, error), signed authority.Receipt, raw []byte) error {
	if CheckProductionHandoff(ctx, tx, load, signed, raw) != nil {
		return ErrExecution
	}
	var c productioncompletion.Command
	if authority.DecodeStrict(raw, &c) != nil {
		return ErrExecution
	}
	if _, e := tx.ExecContext(ctx, `INSERT INTO production_handoffs(terminal_id,command_id,payload_digest)VALUES($1,$2,$3)`, c.Fact.TerminalID, c.ID(), authority.Digest(raw)); e != nil {
		return ErrExecution
	}
	return CheckProductionHandoff(ctx, tx, load, signed, raw)
}

// RecordProductionTx owns only orchestrator facts and its original work item.
// Issue ingestion may have marked that item blocked after our own close ACK.
// No current open-issue predicate or expired execution authority is reused.
func RecordProductionTx(ctx context.Context, tx *sql.Tx, load func() (authority.Policy, error), signed authority.Receipt, raw []byte) error {
	if tx == nil || load == nil || AssertComponent(ctx, tx, "orchestrator") != nil {
		return ErrExecution
	}
	p, e := load()
	if e != nil {
		return ErrExecution
	}
	c, e := productioncompletion.Verify(p, signed, raw)
	if e != nil {
		return ErrExecution
	}
	r, e := readTerminalRecord(ctx, tx, c.Fact.TerminalID, "orchestrator")
	if e != nil || ProductionFact(r) != c.Fact {
		return ErrExecution
	}
	var exists bool
	if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM production_handoffs WHERE terminal_id=$1 AND command_id=$2 AND payload_digest=$3)`, c.Fact.TerminalID, c.ID(), authority.Digest(raw)).Scan(&exists) != nil || !exists {
		return ErrExecution
	}
	var repository, state string
	var issues []byte
	if tx.QueryRowContext(ctx, `SELECT p.repository,w.status,w.issue_numbers FROM work_items w JOIN plans p ON p.id=w.plan_id WHERE w.uuid=$1 FOR UPDATE OF w`, c.Fact.WorkItemID).Scan(&repository, &state, &issues) != nil || repository != c.Fact.Repository {
		return ErrExecution
	}
	var nums []int
	if json.Unmarshal(issues, &nums) != nil || len(nums) != 1 || nums[0] != c.Fact.IssueNumber {
		return ErrExecution
	}
	if state == "completed" {
		if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM production_records WHERE terminal_id=$1 AND work_item_id=$2 AND record_digest=$3 AND command_id=$4 AND payload_digest=$5)`, c.Fact.TerminalID, c.Fact.WorkItemID, c.Fact.TerminalRecordSHA256, c.ID(), authority.Digest(raw)).Scan(&exists) != nil || !exists {
			return ErrExecution
		}
	} else {
		if state != "running" && state != "blocked" {
			return ErrExecution
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO production_records(terminal_id,work_item_id,record_digest,command_id,payload_digest)VALUES($1,$2,$3,$4,$5)`, c.Fact.TerminalID, c.Fact.WorkItemID, c.Fact.TerminalRecordSHA256, c.ID(), authority.Digest(raw)); e != nil {
			return ErrExecution
		}
		result, e := tx.ExecContext(ctx, `UPDATE work_items SET status='completed',updated_at=clock_timestamp() WHERE uuid=$1 AND status IN('running','blocked')`, c.Fact.WorkItemID)
		if e != nil {
			return ErrExecution
		}
		n, e := result.RowsAffected()
		if e != nil || n != 1 {
			return ErrExecution
		}
	}
	// Revalidate after both SQL waits, without requiring fresh/open ingress.
	p, e = load()
	if e != nil || ctx.Err() != nil || c.Validate(p, r.CompletedAt) != nil {
		return ErrExecution
	}
	current, e := readTerminalRecord(ctx, tx, c.Fact.TerminalID, "orchestrator")
	if e != nil || current.Digest() != r.Digest() {
		return ErrExecution
	}
	return nil
}
