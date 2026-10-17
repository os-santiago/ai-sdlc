package planning

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strconv"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/outbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// Deps carries this component's optional external dependencies. A zero
// Deps (DB == nil) leaves /v1/plans/reconcile exactly as ADR-0004 shipped
// it (stateless dry run). The two event-ingress endpoints
// (issueAdmissionDecided, implementationAttemptDecided) require Deps.DB —
// they exist to maintain cross-call saga state (ADR-0002), which has
// nowhere to live without it.
type Deps struct {
	DB *store.DB
}

// issueAdmissionDecided mirrors
// contracts/events/v1/issue-admission-decided.schema.json.
type issueAdmissionDecided struct {
	CorrelationID      string   `json:"correlationId"`
	Repository         string   `json:"repository"`
	Issue              int      `json:"issue"`
	IssueRevision      string   `json:"issueRevision"`
	Decision           string   `json:"decision"`
	RequiredGates      []string `json:"requiredGates"`
	AcceptanceCriteria []string `json:"acceptanceCriteria"`
}

// implementationAttemptDecided mirrors
// contracts/events/v1/implementation-attempt-decided.schema.json.
type implementationAttemptDecided struct {
	CorrelationID string `json:"correlationId"`
	WorkItemID    string `json:"workItemId"`
	AttemptNumber int    `json:"attemptNumber"`
	Outcome       string `json:"outcome"`
	Reason        string `json:"reason"`
	Reprompt      bool   `json:"reprompt"`
}

// workItemReady mirrors contracts/commands/v1/work-item-ready.schema.json.
type workItemReady struct {
	CommandID          string   `json:"commandId"`
	IdempotencyKey     string   `json:"idempotencyKey"`
	CorrelationID      string   `json:"correlationId"`
	PlanID             string   `json:"planId"`
	WorkItemID         string   `json:"workItemId"`
	Repository         string   `json:"repository"`
	BaseRef            string   `json:"baseRef"`
	IssueNumbers       []int    `json:"issueNumbers"`
	AcceptanceCriteria []string `json:"acceptanceCriteria"`
	RequiredGates      []string `json:"requiredGates"`
	LeaseSeconds       int      `json:"leaseSeconds"`
}

// Routes registers the orchestrator's HTTP API: the original dry-run
// /v1/plans/reconcile (unchanged, no persistence — ADR-0004), plus two
// event-ingress endpoints added in ADR-0005 that a relay (internal/outbox)
// delivers IssueAdmissionDecided.v1 and ImplementationAttemptDecided.v1 to.
func Routes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("POST /v1/plans/reconcile", func(w http.ResponseWriter, r *http.Request) {
		var request reconcileRequest
		if !service.DecodeJSON(w, r, &request) {
			return
		}
		plan, ready, err := Reconcile(request.Plan, request.MaxConcurrent)
		if err != nil {
			service.WriteJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		service.WriteJSON(w, http.StatusOK, map[string]any{"plan": plan, "ready": ready})
	})

	mux.HandleFunc("POST /v1/plans/events/issue-admission-decided", func(w http.ResponseWriter, r *http.Request) {
		if deps.DB == nil {
			service.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "orchestrator event ingress requires a database; set DATABASE_URL"})
			return
		}
		var event issueAdmissionDecided
		if !service.DecodeJSON(w, r, &event) {
			return
		}
		ready, err := handleIssueAdmissionDecided(r.Context(), deps.DB, event)
		if err != nil {
			service.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		service.WriteJSON(w, http.StatusOK, map[string]any{"dispatched": ready})
	})

	mux.HandleFunc("POST /v1/plans/events/implementation-attempt-decided", func(w http.ResponseWriter, r *http.Request) {
		if deps.DB == nil {
			service.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "orchestrator event ingress requires a database; set DATABASE_URL"})
			return
		}
		var event implementationAttemptDecided
		if !service.DecodeJSON(w, r, &event) {
			return
		}
		if err := handleImplementationAttemptDecided(r.Context(), deps.DB, event); err != nil {
			service.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		service.WriteJSON(w, http.StatusOK, map[string]string{"status": "acknowledged"})
	})

	// ReleaseGateDecided.v1 is in internal/outbox.DefaultRoutes (routed to
	// here) but not yet reconciled against plan/work-item state: doing so
	// needs a pull-request-to-work-item mapping this runtime does not
	// persist yet (no component here opens a PR — see ADR-0005
	// "Explicitly still out of scope", StubEngine never returns a
	// PullRequestURL). Acknowledging with 200 and no state change avoids
	// an indefinite relay retry loop for a route that otherwise 404s;
	// release-manager's own persistence (internal/store.ReleaseGateRecord)
	// remains the durable record of this decision in the meantime.
	mux.HandleFunc("POST /v1/plans/events/release-gate-decided", func(w http.ResponseWriter, r *http.Request) {
		var discard map[string]any
		if !service.DecodeJSON(w, r, &discard) {
			return
		}
		service.WriteJSON(w, http.StatusOK, map[string]string{"status": "acknowledged"})
	})
}

// handleIssueAdmissionDecided upserts (or reuses) a repository-scoped plan
// and the work item for one accepted issue, reconciles readiness, and
// appends a WorkItemReady.v1 command for every item the reconcile just
// unblocked. Non-"accepted" decisions are acknowledged with no plan
// mutation — there is nothing to schedule.
func handleIssueAdmissionDecided(ctx context.Context, db *store.DB, event issueAdmissionDecided) ([]string, error) {
	if event.Decision != "accepted" {
		return nil, nil
	}

	var dispatchedWorkItemIDs []string
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		plan, err := getOrCreatePlan(ctx, tx, event.Repository, event.CorrelationID)
		if err != nil {
			return err
		}

		workItemHumanID := fmt.Sprintf("issue-%d", event.Issue)
		existing, err := getWorkItem(ctx, tx, plan.ID, workItemHumanID)
		if err != nil && err != store.ErrNotFound {
			return err
		}
		if err == store.ErrNotFound {
			if err := store.UpsertWorkItem(ctx, tx, store.WorkItemRecord{
				PlanID:             plan.ID,
				ID:                 workItemHumanID,
				UUID:               store.NewID(),
				IssueNumbers:       []int{event.Issue},
				AcceptanceCriteria: nonEmptyOrPlaceholder(event.AcceptanceCriteria, event.Issue),
				DependsOn:          []string{},
				Status:             string(Pending),
			}); err != nil {
				return err
			}
		} else {
			// Idempotent redelivery of the same admitted issue: never
			// regress an already-progressed work item back to Pending.
			_ = existing
		}

		records, err := store.ListWorkItemsByPlan(ctx, tx, plan.ID)
		if err != nil {
			return err
		}
		workPlan, idToUUID := toWorkPlan(plan.ID, event.Repository, records)

		_, ready, err := Reconcile(workPlan, maxConcurrentFromEnv())
		if err != nil {
			return fmt.Errorf("reconcile plan %s: %w", plan.ID, err)
		}

		for _, item := range ready {
			uuid := idToUUID[item.ID]
			if err := store.UpdateWorkItemStatus(ctx, tx, uuid, string(Running)); err != nil {
				return fmt.Errorf("mark work item %s running: %w", item.ID, err)
			}
			if err := outbox.Append(ctx, tx, outbox.Record{
				AggregateType: "work_item",
				AggregateID:   uuid,
				EventType:     "WorkItemReady.v1",
				CorrelationID: plan.CorrelationID,
				Payload: workItemReady{
					CommandID:          store.NewID(),
					IdempotencyKey:     uuid,
					CorrelationID:      plan.CorrelationID,
					PlanID:             plan.ID,
					WorkItemID:         uuid,
					Repository:         event.Repository,
					BaseRef:            baseRefFromEnv(),
					IssueNumbers:       item.IssueNumbers,
					AcceptanceCriteria: item.AcceptanceCriteria,
					RequiredGates:      event.RequiredGates,
					LeaseSeconds:       leaseSecondsFromEnv(),
				},
			}); err != nil {
				return err
			}
			dispatchedWorkItemIDs = append(dispatchedWorkItemIDs, uuid)
		}
		return nil
	})
	return dispatchedWorkItemIDs, err
}

// handleImplementationAttemptDecided updates the owning work item's status
// on a terminal outcome (completed/failed/needs-human). "continue" is
// acknowledged with no state change: the worker component owns retrying
// within its own bounded loop (ADR-0003) and will emit another
// ImplementationAttemptDecided.v1 for the next attempt itself.
func handleImplementationAttemptDecided(ctx context.Context, db *store.DB, event implementationAttemptDecided) error {
	var status Status
	switch event.Outcome {
	case "completed":
		status = Completed
	case "failed":
		status = Failed
	case "needs-human":
		status = Blocked
	default:
		return nil // "continue": nothing to persist here.
	}
	err := store.UpdateWorkItemStatus(ctx, db.Pool, event.WorkItemID, string(status))
	if err == store.ErrNotFound {
		// Redelivery for a work item this orchestrator instance never
		// created (e.g. after a restore from an older backup) — log-worthy
		// but not fatal; nothing to compensate.
		return nil
	}
	return err
}

func getOrCreatePlan(ctx context.Context, tx *sql.Tx, repository, correlationID string) (store.PlanRecord, error) {
	row := tx.QueryRowContext(ctx, `SELECT id, repository, correlation_id FROM plans WHERE repository = $1`, repository)
	var plan store.PlanRecord
	err := row.Scan(&plan.ID, &plan.Repository, &plan.CorrelationID)
	if err == nil {
		return plan, nil
	}
	if err != sql.ErrNoRows {
		return store.PlanRecord{}, fmt.Errorf("look up plan for %s: %w", repository, err)
	}
	plan = store.PlanRecord{ID: store.NewID(), Repository: repository, CorrelationID: correlationID}
	if err := store.UpsertPlan(ctx, tx, plan); err != nil {
		return store.PlanRecord{}, err
	}
	return plan, nil
}

func getWorkItem(ctx context.Context, tx *sql.Tx, planID, id string) (store.WorkItemRecord, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT plan_id, id, uuid, issue_numbers, acceptance_criteria, depends_on, status
		FROM work_items WHERE plan_id = $1 AND id = $2
	`, planID, id)
	var rec store.WorkItemRecord
	var issueNumbers, acceptanceCriteria, dependsOn []byte
	if err := row.Scan(&rec.PlanID, &rec.ID, &rec.UUID, &issueNumbers, &acceptanceCriteria, &dependsOn, &rec.Status); err != nil {
		if err == sql.ErrNoRows {
			return store.WorkItemRecord{}, store.ErrNotFound
		}
		return store.WorkItemRecord{}, err
	}
	return rec, nil
}

func toWorkPlan(planID, repository string, records []store.WorkItemRecord) (Plan, map[string]string) {
	items := make([]WorkItem, 0, len(records))
	idToUUID := make(map[string]string, len(records))
	for _, rec := range records {
		items = append(items, WorkItem{
			ID:                 rec.ID,
			IssueNumbers:       rec.IssueNumbers,
			AcceptanceCriteria: rec.AcceptanceCriteria,
			DependsOn:          rec.DependsOn,
			Status:             Status(rec.Status),
		})
		idToUUID[rec.ID] = rec.UUID
	}
	return Plan{ID: planID, Repository: repository, Items: items}, idToUUID
}

// nonEmptyOrPlaceholder guards planning.Validate's "work item is incomplete"
// rule: IssueAdmissionDecided.v1 only started carrying acceptanceCriteria
// in ADR-0005 (see contracts/events/v1/issue-admission-decided.schema.json);
// an admission-controller still running the ADR-0004 payload shape (no
// acceptanceCriteria field) would otherwise produce an unschedulable plan.
func nonEmptyOrPlaceholder(criteria []string, issue int) []string {
	if len(criteria) > 0 {
		return criteria
	}
	return []string{fmt.Sprintf("See issue #%d — acceptance criteria were not carried on this IssueAdmissionDecided.v1 event.", issue)}
}

func maxConcurrentFromEnv() int {
	return intFromEnv("AI_SDLC_ORCHESTRATOR_MAX_CONCURRENT", 1)
}

func leaseSecondsFromEnv() int {
	return intFromEnv("AI_SDLC_WORKER_LEASE_SECONDS", 1800)
}

func baseRefFromEnv() string {
	if ref := os.Getenv("AI_SDLC_BASE_REF"); ref != "" {
		return ref
	}
	return "main"
}

func intFromEnv(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return fallback
	}
	return value
}
