package planning

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// Exercise the strict HTTP boundary, not just the domain functions. These
// fields are emitted by admission.persist and cycle.persistAttempt in real runs.
func TestRoutesAcceptCompleteRuntimeEvents(t *testing.T) {
	db := testDB(t)
	mux := http.NewServeMux()
	Routes(mux, Deps{DB: db})
	post := func(path string, event map[string]any, want int) {
		t.Helper()
		body, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
		if response.Code != want {
			t.Fatalf("%s: status %d, body %s", path, response.Code, response.Body.String())
		}
	}
	repository := "local/wire-" + store.NewID()
	admitted := map[string]any{
		"eventId": store.NewID(), "occurredAt": "2026-11-14T00:00:00Z", "correlationId": "wire-test",
		"repository": repository, "issue": 1, "issueRevision": "revision-1", "decision": "accepted",
		"risk": "low", "policyVersion": "1.0.0", "reasons": []string{"Accepted."},
		"requiredGates": []string{"tests"}, "acceptanceCriteria": []string{"Document the local test procedure."},
	}
	post("/v1/plans/events/issue-admission-decided", admitted, http.StatusOK)
	var id string
	if err := db.Pool.QueryRowContext(context.Background(), `SELECT w.uuid FROM work_items w JOIN plans p ON p.id=w.plan_id WHERE p.repository=$1`, repository).Scan(&id); err != nil {
		t.Fatal(err)
	}
	post("/v1/plans/events/implementation-attempt-decided", map[string]any{
		"eventId": store.NewID(), "occurredAt": "2026-11-14T00:00:01Z", "correlationId": "wire-test",
		"workItemId": id, "attemptNumber": 1, "outcome": "completed", "reason": "Validated.", "reprompt": false,
	}, http.StatusOK)
	var status string
	if err := db.Pool.QueryRowContext(context.Background(), `SELECT status FROM work_items WHERE uuid=$1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "completed" {
		t.Fatalf("work item status = %s", status)
	}
	admitted["unexpectedAuthority"] = true
	post("/v1/plans/events/issue-admission-decided", admitted, http.StatusBadRequest)
}

func testDB(t *testing.T) *store.DB {
	t.Helper()
	url := os.Getenv("AI_SDLC_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AI_SDLC_TEST_DATABASE_URL not set; skipping Postgres-backed test")
	}
	db, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return db
}

func TestHandleIssueAdmissionDecidedSkipsNonAcceptedDecision(t *testing.T) {
	// No database needed: handleIssueAdmissionDecided returns before
	// touching the store for any decision other than "accepted".
	ready, err := handleIssueAdmissionDecided(context.Background(), nil, issueAdmissionDecided{
		Repository: "os-santiago/homedir",
		Issue:      1,
		Decision:   "needs-human",
	})
	if err != nil {
		t.Fatalf("handleIssueAdmissionDecided() error = %v", err)
	}
	if ready != nil {
		t.Fatalf("ready = %v, want nil for a non-accepted decision", ready)
	}
}

func TestHandleIssueAdmissionDecidedCreatesWorkItemAndDispatches(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	event := issueAdmissionDecided{
		CorrelationID:      "os-santiago/homedir#1573@rev-1",
		Repository:         "os-santiago/homedir-events-test",
		Issue:              1573,
		IssueRevision:      "rev-1",
		Decision:           "accepted",
		RequiredGates:      []string{"tests", "quality"},
		AcceptanceCriteria: []string{"The thing is fixed."},
	}
	ready, err := handleIssueAdmissionDecided(ctx, db, event)
	if err != nil {
		t.Fatalf("handleIssueAdmissionDecided() error = %v", err)
	}
	if len(ready) != 1 {
		t.Fatalf("dispatched work items = %v, want exactly 1", ready)
	}

	var status string
	row := db.Pool.QueryRowContext(ctx, `SELECT status FROM work_items WHERE uuid = $1`, ready[0])
	if err := row.Scan(&status); err != nil {
		t.Fatalf("read back work item: %v", err)
	}
	if status != string(Running) {
		t.Fatalf("status = %q, want %q (dispatched work items move straight to Running)", status, Running)
	}

	var outboxCount int
	row = db.Pool.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE event_type = 'WorkItemReady.v1' AND aggregate_id = $1`, ready[0])
	if err := row.Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if outboxCount != 1 {
		t.Fatalf("WorkItemReady.v1 outbox rows = %d, want 1", outboxCount)
	}

	// Redelivering the same admitted issue must not duplicate the work
	// item or re-dispatch a second WorkItemReady.v1 command.
	ready2, err := handleIssueAdmissionDecided(ctx, db, event)
	if err != nil {
		t.Fatalf("second handleIssueAdmissionDecided() error = %v", err)
	}
	if len(ready2) != 0 {
		t.Fatalf("second dispatch = %v, want empty (already Running, not re-readied)", ready2)
	}
}

func TestHandleImplementationAttemptDecidedUpdatesStatus(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	event := issueAdmissionDecided{
		CorrelationID:      "corr-2",
		Repository:         "os-santiago/homedir-events-test-2",
		Issue:              42,
		IssueRevision:      "rev-1",
		Decision:           "accepted",
		AcceptanceCriteria: []string{"Done."},
	}
	ready, err := handleIssueAdmissionDecided(ctx, db, event)
	if err != nil {
		t.Fatalf("handleIssueAdmissionDecided() error = %v", err)
	}
	if len(ready) != 1 {
		t.Fatalf("dispatched work items = %v, want exactly 1", ready)
	}
	workItemUUID := ready[0]

	if err := handleImplementationAttemptDecided(ctx, db, implementationAttemptDecided{
		WorkItemID:    workItemUUID,
		AttemptNumber: 1,
		Outcome:       "completed",
		Reason:        "Implementation and scoped validation succeeded.",
	}); err != nil {
		t.Fatalf("handleImplementationAttemptDecided() error = %v", err)
	}

	var status string
	row := db.Pool.QueryRowContext(ctx, `SELECT status FROM work_items WHERE uuid = $1`, workItemUUID)
	if err := row.Scan(&status); err != nil {
		t.Fatalf("read back work item: %v", err)
	}
	if status != string(Completed) {
		t.Fatalf("status = %q, want %q", status, Completed)
	}
}

func TestHandleImplementationAttemptDecidedIgnoresContinueOutcome(t *testing.T) {
	if err := handleImplementationAttemptDecided(context.Background(), nil, implementationAttemptDecided{
		WorkItemID:    "does-not-matter",
		AttemptNumber: 1,
		Outcome:       "continue",
	}); err != nil {
		t.Fatalf("handleImplementationAttemptDecided() error = %v, want nil (continue is a no-op, no DB access)", err)
	}
}
