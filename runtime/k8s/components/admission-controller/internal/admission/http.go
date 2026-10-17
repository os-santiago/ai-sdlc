package admission

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

type evaluateRequest struct {
	Issue  Issue  `json:"issue"`
	Policy Policy `json:"policy"`
	// CorrelationID carries a caller-supplied correlation id through the
	// IssueAdmissionDecided.v1 event this handler emits when Deps.DB is
	// configured. Defaults to "<repository>#<issue>@<revision>" so a dry
	// run (no DB, no correlation id supplied) and the wired pipeline
	// address the same issue consistently.
	CorrelationID string `json:"correlationId"`
}

// Deps carries this component's optional external dependencies. A zero
// Deps (DB == nil) is exactly ADR-0004's original behavior: evaluate and
// respond, no persistence, no outbox event — used by every existing unit
// test and by any dry run that sets no AI_SDLC_DATABASE_URL.
type Deps struct {
	DB *store.DB
}

// Routes registers the admission-controller's HTTP API. POST
// /v1/admissions/evaluate accepts an explicit policy (useful for dry runs
// and tests) or, when omitted, loads it from the .ai-sdlc.yaml path named by
// AI_SDLC_CONTRACT_PATH (falling back to the schema-documented defaults when
// unset or unreadable). When deps.DB is set, the decision is additionally
// persisted (idempotent upsert, internal/store.UpsertAdmission) and an
// IssueAdmissionDecided.v1 event is appended to the outbox in the same
// transaction (ADR-0005) — the pure Evaluate call itself is unchanged.
func Routes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("POST /v1/admissions/evaluate", func(w http.ResponseWriter, r *http.Request) {
		var request evaluateRequest
		if !service.DecodeJSON(w, r, &request) {
			return
		}
		if request.Policy.Version == "" {
			request.Policy = PolicyFromContract(contract.LoadFromEnv())
		}
		result := Evaluate(request.Issue, request.Policy)

		if deps.DB != nil {
			correlationID := request.CorrelationID
			if correlationID == "" {
				correlationID = defaultCorrelationID(request.Issue)
			}
			if err := persist(r.Context(), deps.DB, request.Issue, result, correlationID); err != nil {
				service.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
		}
		service.WriteJSON(w, http.StatusOK, result)
	})
}

func defaultCorrelationID(issue Issue) string {
	return fmt.Sprintf("%s#%d@%s", issue.Repository, issue.Number, issue.Revision)
}

// persist records the admission decision and appends the
// IssueAdmissionDecided.v1 event atomically.
func persist(ctx context.Context, db *store.DB, issue Issue, result Result, correlationID string) error {
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := store.UpsertAdmission(ctx, tx, store.AdmissionRecord{
			Repository:    issue.Repository,
			IssueNumber:   issue.Number,
			IssueRevision: issue.Revision,
			Decision:      string(result.Decision),
			Risk:          result.Risk,
			PolicyVersion: result.PolicyVersion,
			Reasons:       result.Reasons,
			RequiredGates: result.RequiredGates,
			CorrelationID: correlationID,
		}); err != nil {
			return err
		}
		return outbox.Append(ctx, tx, outbox.Record{
			AggregateType: "admission",
			AggregateID:   fmt.Sprintf("%s#%d@%s", issue.Repository, issue.Number, issue.Revision),
			EventType:     "IssueAdmissionDecided.v1",
			CorrelationID: correlationID,
			Payload: map[string]any{
				"eventId":            store.NewID(),
				"occurredAt":         time.Now().UTC().Format(time.RFC3339),
				"correlationId":      correlationID,
				"repository":         issue.Repository,
				"issue":              issue.Number,
				"issueRevision":      issue.Revision,
				"decision":           result.Decision,
				"risk":               result.Risk,
				"policyVersion":      result.PolicyVersion,
				"reasons":            result.Reasons,
				"requiredGates":      result.RequiredGates,
				"acceptanceCriteria": issue.AcceptanceCriteria,
			},
		})
	})
}
