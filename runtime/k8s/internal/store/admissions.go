package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// AdmissionRecord is the durable form of one admission-controller decision
// (components/admission-controller/internal/admission.Result), keyed so
// redelivering the same normalized event is an upsert, not a duplicate.
type AdmissionRecord struct {
	Repository    string
	IssueNumber   int
	IssueRevision string
	Decision      string
	Risk          string
	PolicyVersion string
	Reasons       []string
	RequiredGates []string
	CorrelationID string
}

// UpsertAdmission records (or re-records, idempotently) an admission
// decision. Call within a *DB.WithTx alongside internal/outbox.Append so
// the decision and its IssueAdmissionDecided.v1 event are atomic.
func UpsertAdmission(ctx context.Context, q Querier, rec AdmissionRecord) error {
	reasons, err := json.Marshal(rec.Reasons)
	if err != nil {
		return fmt.Errorf("marshal reasons: %w", err)
	}
	gates, err := json.Marshal(rec.RequiredGates)
	if err != nil {
		return fmt.Errorf("marshal required gates: %w", err)
	}
	_, err = q.ExecContext(ctx, `
		INSERT INTO admissions (repository, issue_number, issue_revision, decision, risk, policy_version, reasons, required_gates, correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (repository, issue_number, issue_revision) DO UPDATE SET
			decision = EXCLUDED.decision,
			risk = EXCLUDED.risk,
			policy_version = EXCLUDED.policy_version,
			reasons = EXCLUDED.reasons,
			required_gates = EXCLUDED.required_gates,
			correlation_id = EXCLUDED.correlation_id,
			updated_at = now()
	`, rec.Repository, rec.IssueNumber, rec.IssueRevision, rec.Decision, rec.Risk, rec.PolicyVersion, reasons, gates, rec.CorrelationID)
	if err != nil {
		return fmt.Errorf("upsert admission: %w", err)
	}
	return nil
}
