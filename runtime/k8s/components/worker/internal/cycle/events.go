package cycle

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/contract"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/engine"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/outbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// Deps carries this component's optional external dependencies. A zero
// Deps (DB == nil) leaves /v1/cycles/evaluate exactly as ADR-0004 shipped
// it. /v1/attempts/dispatch (ADR-0005) requires both DB (attempt history
// must survive a pod restart — ADR-0003's "every attempt loop must
// terminate" needs a durable attempt count to resume from) and Engine
// (defaults to engine.StubEngine{} when nil, never requires one — see
// components/worker/cmd/main.go).
type Deps struct {
	DB     *store.DB
	Engine engine.Engine
}

// workItemReady mirrors contracts/commands/v1/work-item-ready.schema.json
// — the command internal/outbox.Relay delivers here from the orchestrator.
type workItemReady struct {
	CorrelationID      string   `json:"correlationId"`
	PlanID             string   `json:"planId"`
	WorkItemID         string   `json:"workItemId"`
	Repository         string   `json:"repository"`
	BaseRef            string   `json:"baseRef"`
	IssueNumbers       []int    `json:"issueNumbers"`
	AcceptanceCriteria []string `json:"acceptanceCriteria"`
}

// Routes registers the worker's HTTP API: the original dry-run
// /v1/cycles/evaluate (unchanged), plus /v1/attempts/dispatch (ADR-0005),
// which runs the full bounded-edit loop (ADR-0003) for one work item:
// invoke the engine, evaluate the attempt, persist it and emit
// ImplementationAttemptDecided.v1, repeating until a terminal outcome
// (completed/failed/needs-human) or the policy's attempt ceiling.
func Routes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("POST /v1/cycles/evaluate", func(w http.ResponseWriter, r *http.Request) {
		var request evaluateRequest
		if !service.DecodeJSON(w, r, &request) {
			return
		}
		if request.Policy.MaximumAttempts == 0 {
			request.Policy = PolicyFromContract(contract.LoadFromEnv())
		}
		decision, err := Evaluate(request.Attempt, request.Policy)
		if err != nil {
			service.WriteJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		service.WriteJSON(w, http.StatusOK, decision)
	})

	mux.HandleFunc("POST /v1/attempts/dispatch", func(w http.ResponseWriter, r *http.Request) {
		if deps.DB == nil {
			service.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "worker attempt dispatch requires a database; set DATABASE_URL"})
			return
		}
		var command workItemReady
		if !service.DecodeJSON(w, r, &command) {
			return
		}
		eng := deps.Engine
		if eng == nil {
			eng = engine.StubEngine{}
		}
		policy := PolicyFromContract(contract.LoadFromEnv())
		decision, attemptNumber, err := dispatch(r.Context(), deps.DB, eng, command, policy)
		if err != nil {
			service.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		service.WriteJSON(w, http.StatusOK, map[string]any{"decision": decision, "attemptNumber": attemptNumber})
	})
}

// dispatch runs the bounded-edit loop for one work item: resume from the
// durable attempt count, invoke the engine, evaluate, persist and emit —
// repeat until Evaluate returns anything other than Continue, or until
// policy.MaximumAttempts attempts have run in this one call (Evaluate
// itself escalates at the ceiling, so this is a defensive backstop, never
// expected to be the thing that actually stops the loop).
func dispatch(ctx context.Context, db *store.DB, eng engine.Engine, command workItemReady, policy Policy) (Decision, int, error) {
	completed, err := store.CountAttempts(ctx, db.Pool, command.WorkItemID)
	if err != nil {
		return Decision{}, 0, fmt.Errorf("count prior attempts for work item %s: %w", command.WorkItemID, err)
	}

	var lastDecision Decision
	attemptNumber := completed + 1
	for loop := 0; loop <= policy.MaximumAttempts && attemptNumber <= policy.MaximumAttempts; loop++ {
		result, err := eng.Invoke(ctx, engine.Request{
			WorkItemID:         command.WorkItemID,
			Repository:         command.Repository,
			BaseRef:            command.BaseRef,
			IssueNumbers:       command.IssueNumbers,
			AcceptanceCriteria: command.AcceptanceCriteria,
			AttemptNumber:      attemptNumber,
		})
		if err != nil {
			return Decision{}, attemptNumber, fmt.Errorf("engine invoke attempt %d for work item %s: %w", attemptNumber, command.WorkItemID, err)
		}

		attempt := Attempt{
			Number:             attemptNumber,
			ExitCode:           result.ExitCode,
			TimedOut:           result.TimedOut,
			ChangedFiles:       result.ChangedFiles,
			ValidationPassed:   result.ValidationPassed,
			ActionableFeedback: result.ActionableFeedback,
			UnsafeOrAmbiguous:  result.UnsafeOrAmbiguous,
		}
		decision, err := Evaluate(attempt, policy)
		if err != nil {
			return Decision{}, attemptNumber, fmt.Errorf("evaluate attempt %d for work item %s: %w", attemptNumber, command.WorkItemID, err)
		}
		lastDecision = decision

		if err := persistAttempt(ctx, db, command, attempt, decision); err != nil {
			return decision, attemptNumber, err
		}

		if decision.Outcome != Continue {
			return decision, attemptNumber, nil
		}
		attemptNumber++
	}
	return lastDecision, attemptNumber - 1, nil
}

func persistAttempt(ctx context.Context, db *store.DB, command workItemReady, attempt Attempt, decision Decision) error {
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := store.InsertAttempt(ctx, tx, store.AttemptRecord{
			WorkItemUUID:       command.WorkItemID,
			AttemptNumber:      attempt.Number,
			ExitCode:           attempt.ExitCode,
			TimedOut:           attempt.TimedOut,
			ChangedFiles:       attempt.ChangedFiles,
			ValidationPassed:   attempt.ValidationPassed,
			ActionableFeedback: attempt.ActionableFeedback,
			UnsafeOrAmbiguous:  attempt.UnsafeOrAmbiguous,
			Outcome:            string(decision.Outcome),
			Reason:             decision.Reason,
			Reprompt:           decision.Reprompt,
			CorrelationID:      command.CorrelationID,
		}); err != nil {
			return err
		}
		return outbox.Append(ctx, tx, outbox.Record{
			AggregateType: "attempt",
			AggregateID:   command.WorkItemID,
			EventType:     "ImplementationAttemptDecided.v1",
			CorrelationID: command.CorrelationID,
			Payload: map[string]any{
				"eventId":       store.NewID(),
				"occurredAt":    time.Now().UTC().Format(time.RFC3339),
				"correlationId": command.CorrelationID,
				"workItemId":    command.WorkItemID,
				"attemptNumber": attempt.Number,
				"outcome":       decision.Outcome,
				"reason":        decision.Reason,
				"reprompt":      decision.Reprompt,
			},
		})
	})
}
