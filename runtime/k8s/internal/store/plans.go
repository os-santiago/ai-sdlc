package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNotFound is returned by single-row lookups when no row matches.
var ErrNotFound = errors.New("store: not found")

// PlanRecord is the durable form of one orchestrator plan
// (components/orchestrator/internal/planning.Plan), scoped to one
// repository's saga (ADR-0002: "dependency-aware sagas with compensating
// actions").
type PlanRecord struct {
	ID            string
	Repository    string
	CorrelationID string
}

// WorkItemRecord is the durable form of one planning.WorkItem. UUID is the
// globally unique identity carried in WorkItemReady.v1 /
// ImplementationAttemptDecided.v1 (schema: workItemId); ID is the
// plan-scoped human key planning.WorkItem.ID uses for DependsOn.
type WorkItemRecord struct {
	PlanID             string
	ID                 string
	UUID               string
	IssueNumbers       []int
	AcceptanceCriteria []string
	DependsOn          []string
	Status             string
}

// UpsertPlan records (or re-records) a plan's identity. Call within the
// same transaction as UpsertWorkItem and internal/outbox.Append.
func UpsertPlan(ctx context.Context, q Querier, rec PlanRecord) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO plans (id, repository, correlation_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (id) DO UPDATE SET updated_at = now()
	`, rec.ID, rec.Repository, rec.CorrelationID)
	if err != nil {
		return fmt.Errorf("upsert plan: %w", err)
	}
	return nil
}

// UpsertWorkItem records (or re-records, idempotently on plan_id+id) one
// work item's current status and shape.
func UpsertWorkItem(ctx context.Context, q Querier, rec WorkItemRecord) error {
	issueNumbers, err := json.Marshal(rec.IssueNumbers)
	if err != nil {
		return fmt.Errorf("marshal issue numbers: %w", err)
	}
	acceptanceCriteria, err := json.Marshal(rec.AcceptanceCriteria)
	if err != nil {
		return fmt.Errorf("marshal acceptance criteria: %w", err)
	}
	dependsOn, err := json.Marshal(rec.DependsOn)
	if err != nil {
		return fmt.Errorf("marshal depends on: %w", err)
	}
	_, err = q.ExecContext(ctx, `
		INSERT INTO work_items (plan_id, id, uuid, issue_numbers, acceptance_criteria, depends_on, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (plan_id, id) DO UPDATE SET
			issue_numbers = EXCLUDED.issue_numbers,
			acceptance_criteria = EXCLUDED.acceptance_criteria,
			depends_on = EXCLUDED.depends_on,
			status = EXCLUDED.status,
			updated_at = now()
	`, rec.PlanID, rec.ID, rec.UUID, issueNumbers, acceptanceCriteria, dependsOn, rec.Status)
	if err != nil {
		return fmt.Errorf("upsert work item: %w", err)
	}
	return nil
}

// GetWorkItemByUUID looks up a work item by its globally unique identity
// (the workItemId carried on WorkItemReady.v1 /
// ImplementationAttemptDecided.v1).
func GetWorkItemByUUID(ctx context.Context, q Querier, uuid string) (WorkItemRecord, error) {
	row := q.QueryRowContext(ctx, `
		SELECT plan_id, id, uuid, issue_numbers, acceptance_criteria, depends_on, status
		FROM work_items WHERE uuid = $1
	`, uuid)
	return scanWorkItem(row)
}

// ListWorkItemsByPlan returns every work item belonging to a plan, the
// shape internal/planning.Reconcile expects as input.
func ListWorkItemsByPlan(ctx context.Context, q Querier, planID string) ([]WorkItemRecord, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT plan_id, id, uuid, issue_numbers, acceptance_criteria, depends_on, status
		FROM work_items WHERE plan_id = $1 ORDER BY id
	`, planID)
	if err != nil {
		return nil, fmt.Errorf("list work items: %w", err)
	}
	defer rows.Close()

	items := make([]WorkItemRecord, 0)
	for rows.Next() {
		item, err := scanWorkItemRow(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// UpdateWorkItemStatus transitions one work item's status by its UUID.
func UpdateWorkItemStatus(ctx context.Context, q Querier, uuid, status string) error {
	result, err := q.ExecContext(ctx, `UPDATE work_items SET status = $1, updated_at = now() WHERE uuid = $2`, status, uuid)
	if err != nil {
		return fmt.Errorf("update work item status: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update work item status: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanWorkItem(row scannable) (WorkItemRecord, error) {
	return scanWorkItemRow(row)
}

func scanWorkItemRow(row scannable) (WorkItemRecord, error) {
	var rec WorkItemRecord
	var issueNumbers, acceptanceCriteria, dependsOn []byte
	if err := row.Scan(&rec.PlanID, &rec.ID, &rec.UUID, &issueNumbers, &acceptanceCriteria, &dependsOn, &rec.Status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkItemRecord{}, ErrNotFound
		}
		return WorkItemRecord{}, fmt.Errorf("scan work item: %w", err)
	}
	if err := json.Unmarshal(issueNumbers, &rec.IssueNumbers); err != nil {
		return WorkItemRecord{}, fmt.Errorf("unmarshal issue numbers: %w", err)
	}
	if err := json.Unmarshal(acceptanceCriteria, &rec.AcceptanceCriteria); err != nil {
		return WorkItemRecord{}, fmt.Errorf("unmarshal acceptance criteria: %w", err)
	}
	if err := json.Unmarshal(dependsOn, &rec.DependsOn); err != nil {
		return WorkItemRecord{}, fmt.Errorf("unmarshal depends on: %w", err)
	}
	return rec, nil
}
