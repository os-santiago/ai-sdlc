package gates

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/contract"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/outbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// evaluateRequest extends Snapshot with the identifiers needed to persist
// a decision and address a ReleaseGateDecided.v1 event — repository and
// pullRequest are not part of gates.Snapshot (which future-go scoped to
// evaluation inputs only), so they travel alongside it on the wire instead
// of widening the pure domain type.
type evaluateRequest struct {
	Snapshot
	Repository    string `json:"repository"`
	PullRequest   int    `json:"pullRequest"`
	CorrelationID string `json:"correlationId"`
}

// Deps carries this component's optional external dependencies. A zero
// Deps (DB == nil) is exactly ADR-0004's original behavior: evaluate and
// respond, no persistence, no outbox event.
type Deps struct {
	DB *store.DB
}

// Routes registers the release-manager's HTTP API. When deps.DB is set,
// the decision is additionally persisted (idempotent upsert,
// internal/store.UpsertReleaseGate) and a ReleaseGateDecided.v1 event is
// appended to the outbox in the same transaction (ADR-0005) — the pure
// Evaluate call itself is unchanged.
func Routes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("POST /v1/releases/evaluate", func(w http.ResponseWriter, r *http.Request) {
		var request evaluateRequest
		if !service.DecodeJSON(w, r, &request) {
			return
		}
		snapshot := ApplyContractDefaults(request.Snapshot, contract.LoadFromEnv())
		result := Evaluate(snapshot)

		if deps.DB != nil && request.Repository != "" && request.PullRequest > 0 {
			correlationID := request.CorrelationID
			if correlationID == "" {
				correlationID = fmt.Sprintf("%s#pr-%d", request.Repository, request.PullRequest)
			}
			if err := persist(r.Context(), deps.DB, request.Repository, request.PullRequest, snapshot, result, correlationID); err != nil {
				service.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
		}
		service.WriteJSON(w, http.StatusOK, result)
	})
}

func persist(ctx context.Context, db *store.DB, repository string, pullRequest int, snapshot Snapshot, result Result, correlationID string) error {
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := store.UpsertReleaseGate(ctx, tx, store.ReleaseGateRecord{
			Repository:          repository,
			PullRequest:         pullRequest,
			State:               string(result.State),
			Reasons:             result.Reasons,
			RemediationAttempts: snapshot.RemediationAttempts,
			CorrelationID:       correlationID,
		}); err != nil {
			return err
		}
		return outbox.Append(ctx, tx, outbox.Record{
			AggregateType: "release_gate",
			AggregateID:   fmt.Sprintf("%s#pr-%d", repository, pullRequest),
			EventType:     "ReleaseGateDecided.v1",
			CorrelationID: correlationID,
			Payload: map[string]any{
				"eventId":       store.NewID(),
				"occurredAt":    time.Now().UTC().Format(time.RFC3339),
				"correlationId": correlationID,
				"repository":    repository,
				"pullRequest":   pullRequest,
				"state":         result.State,
				"reasons":       result.Reasons,
			},
		})
	})
}
