package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// ReleaseGateRecord is the durable form of one release-manager snapshot
// (components/release-manager/internal/gates.Result), keyed so
// redelivering the same CI/review webhook is an upsert, not a duplicate.
type ReleaseGateRecord struct {
	Repository          string
	PullRequest         int
	State               string
	Reasons             []string
	RemediationAttempts int
	CorrelationID       string
}

// UpsertReleaseGate records (or re-records) a release-manager decision.
// Call within the same transaction as internal/outbox.Append
// (ReleaseGateDecided.v1).
func UpsertReleaseGate(ctx context.Context, q Querier, rec ReleaseGateRecord) error {
	reasons, err := json.Marshal(rec.Reasons)
	if err != nil {
		return fmt.Errorf("marshal reasons: %w", err)
	}
	_, err = q.ExecContext(ctx, `
		INSERT INTO release_gates (repository, pull_request, state, reasons, remediation_attempts, correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (repository, pull_request) DO UPDATE SET
			state = EXCLUDED.state,
			reasons = EXCLUDED.reasons,
			remediation_attempts = EXCLUDED.remediation_attempts,
			correlation_id = EXCLUDED.correlation_id,
			updated_at = now()
	`, rec.Repository, rec.PullRequest, rec.State, reasons, rec.RemediationAttempts, rec.CorrelationID)
	if err != nil {
		return fmt.Errorf("upsert release gate: %w", err)
	}
	return nil
}
