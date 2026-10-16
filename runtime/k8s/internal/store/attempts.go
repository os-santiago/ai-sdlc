package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// AttemptRecord is the durable audit trail of one bounded implementation
// attempt: what was fed to components/worker/internal/cycle.Evaluate and
// what it returned. cycle.Evaluate itself stays a pure function; this
// table exists so a crashed/restarted worker pod can recover attempt
// history for a work item instead of losing count of how many bounded
// attempts have already run (ADR-0003: every attempt loop must terminate).
type AttemptRecord struct {
	WorkItemUUID       string
	AttemptNumber      int
	ExitCode           int
	TimedOut           bool
	ChangedFiles       []string
	ValidationPassed   bool
	ActionableFeedback []string
	UnsafeOrAmbiguous  bool
	Outcome            string
	Reason             string
	Reprompt           bool
	CorrelationID      string
}

// InsertAttempt records one attempt. Call within the same transaction as
// internal/outbox.Append (ImplementationAttemptDecided.v1).
func InsertAttempt(ctx context.Context, q Querier, rec AttemptRecord) error {
	changedFiles, err := json.Marshal(rec.ChangedFiles)
	if err != nil {
		return fmt.Errorf("marshal changed files: %w", err)
	}
	actionableFeedback, err := json.Marshal(rec.ActionableFeedback)
	if err != nil {
		return fmt.Errorf("marshal actionable feedback: %w", err)
	}
	_, err = q.ExecContext(ctx, `
		INSERT INTO attempts (work_item_uuid, attempt_number, exit_code, timed_out, changed_files, validation_passed, actionable_feedback, unsafe_or_ambiguous, outcome, reason, reprompt, correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (work_item_uuid, attempt_number) DO NOTHING
	`, rec.WorkItemUUID, rec.AttemptNumber, rec.ExitCode, rec.TimedOut, changedFiles, rec.ValidationPassed, actionableFeedback, rec.UnsafeOrAmbiguous, rec.Outcome, rec.Reason, rec.Reprompt, rec.CorrelationID)
	if err != nil {
		return fmt.Errorf("insert attempt: %w", err)
	}
	return nil
}

// CountAttempts returns how many attempts have already been recorded for a
// work item, so a redispatched WorkItemReady.v1 command resumes at the
// correct attempt number instead of restarting the bounded loop at 1.
func CountAttempts(ctx context.Context, q Querier, workItemUUID string) (int, error) {
	var count int
	row := q.QueryRowContext(ctx, `SELECT count(*) FROM attempts WHERE work_item_uuid = $1`, workItemUUID)
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("count attempts: %w", err)
	}
	return count, nil
}
