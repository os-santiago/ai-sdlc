package store

import (
	"context"
	"time"
)

// ExecutionEvidence is one consistent, bounded SQL snapshot. Its payloads
// still require semantic verification against the signed command and policy.
type ExecutionEvidence struct {
	ObservationAuthority                                                                                   string
	PlanCorrelation, Status, IntakeEvent, IntakeRevision, ConfigDigest, ContentKey                         string
	AdmissionEvent, PlanningEvent, Decision, Risk, PolicyVersion, AdmissionCorrelation                     string
	IntakeGeneration, AdmissionGeneration, PlanningGeneration                                              int64
	IssueNumbers, Criteria, Dependencies, IntakePayload, AdmissionPayload, PlanningPayload, Reasons, Gates []byte
}

func (db *DB) ReadExecutionEvidence(ctx context.Context, work, repository string, issue, maximumAgeSeconds int) (ExecutionEvidence, error) {
	return db.readExecutionEvidence(ctx, "worker", work, repository, issue, maximumAgeSeconds)
}

// ReadVerificationEvidence is read-only and requires the independent identity.
// Its caller must still validate the original signed request and current issue.
func (db *DB) ReadVerificationEvidence(ctx context.Context, work, repository string, issue, maximumAgeSeconds int) (ExecutionEvidence, error) {
	return db.readExecutionEvidence(ctx, "verifier", work, repository, issue, maximumAgeSeconds)
}
func (db *DB) readExecutionEvidence(ctx context.Context, component, work, repository string, issue, maximumAgeSeconds int) (ExecutionEvidence, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var e ExecutionEvidence
	if maximumAgeSeconds < 30 || maximumAgeSeconds > 300 {
		return e, ErrExecution
	}
	if err := AssertComponent(ctx, db.Pool, component); err != nil {
		return e, identityObservationError(err)
	}
	err := db.Pool.QueryRowContext(ctx, `SELECT
        p.correlation_id,w.status,w.issue_numbers,w.acceptance_criteria,w.depends_on,
        ih.generation,ih.content_key,ir.event_id,ir.revision,ir.config_digest,ir.payload,io.authority_revision,
        ah.generation,ah.event_id,ah.payload,ph.generation,ph.event_id,ph.payload,
        a.decision,a.risk,a.policy_version,a.correlation_id,a.reasons,a.required_gates
        FROM work_items w JOIN plans p ON p.id=w.plan_id
        JOIN intake_heads ih ON ih.repository=p.repository AND ih.issue_number=$3
        JOIN intake_revisions ir ON ir.repository=ih.repository AND ir.issue_number=ih.issue_number AND ir.generation=ih.generation
        JOIN intake_cursors ic ON ic.repository=ir.repository AND ic.config_digest=ir.config_digest
        JOIN intake_observations io ON io.repository=ih.repository AND io.issue_number=ih.issue_number AND io.generation=ih.generation AND io.config_digest=ir.config_digest AND io.content_key=ih.content_key
        JOIN admission_heads ah ON ah.repository=ih.repository AND ah.issue_number=ih.issue_number
        JOIN planning_heads ph ON ph.repository=ih.repository AND ph.issue_number=ih.issue_number
        JOIN admissions a ON a.repository=ir.repository AND a.issue_number=ir.issue_number AND a.issue_revision=ir.revision||':'||ir.generation::text
        WHERE w.uuid=$1 AND p.repository=$2
        AND ic.watermark IS NOT NULL AND ic.watermark>=clock_timestamp()-($4*interval '1 second') AND ic.watermark<=clock_timestamp()+interval '5 seconds'
        AND io.observed_at>=clock_timestamp()-($4*interval '1 second') AND io.observed_at<=clock_timestamp()+interval '5 seconds'
        AND octet_length(p.correlation_id)<=128 AND octet_length(w.status)<=32
        AND octet_length(w.issue_numbers::text)<=1024 AND octet_length(w.acceptance_criteria::text)<=65536 AND octet_length(w.depends_on::text)<=16384
        AND octet_length(ih.content_key)<=64 AND octet_length(ir.event_id)<=128 AND octet_length(ir.revision)<=64 AND octet_length(ir.config_digest)<=64
        AND octet_length(ah.event_id)<=128 AND octet_length(ph.event_id)<=128
        AND octet_length(a.decision)<=64 AND octet_length(a.risk)<=32 AND octet_length(a.policy_version)<=64 AND octet_length(a.correlation_id)<=128
        AND octet_length(a.reasons::text)<=65536 AND octet_length(a.required_gates::text)<=16384`, work, repository, issue, maximumAgeSeconds).Scan(
		&e.PlanCorrelation, &e.Status, &e.IssueNumbers, &e.Criteria, &e.Dependencies,
		&e.IntakeGeneration, &e.ContentKey, &e.IntakeEvent, &e.IntakeRevision, &e.ConfigDigest, &e.IntakePayload, &e.ObservationAuthority,
		&e.AdmissionGeneration, &e.AdmissionEvent, &e.AdmissionPayload, &e.PlanningGeneration, &e.PlanningEvent, &e.PlanningPayload,
		&e.Decision, &e.Risk, &e.PolicyVersion, &e.AdmissionCorrelation, &e.Reasons, &e.Gates)
	if err != nil {
		return ExecutionEvidence{}, observationError(err)
	}
	return e, nil
}
