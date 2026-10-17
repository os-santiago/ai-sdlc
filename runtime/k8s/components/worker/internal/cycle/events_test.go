package cycle

import (
	"context"
	"os"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/engine"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

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

// scriptedEngine returns one engine.Result per call, in order, so a test
// can drive the bounded-edit loop (dispatch) through several attempts
// deterministically.
type scriptedEngine struct {
	results []engine.Result
	calls   int
}

func (e *scriptedEngine) Invoke(_ context.Context, _ engine.Request) (engine.Result, error) {
	result := e.results[e.calls]
	e.calls++
	return result, nil
}

func insertWorkItemForDispatch(t *testing.T, db *store.DB, planID, workItemUUID string) {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertPlan(ctx, db.Pool, store.PlanRecord{ID: planID, Repository: "os-santiago/homedir-dispatch-test", CorrelationID: "corr-1"}); err != nil {
		t.Fatalf("upsert plan: %v", err)
	}
	if err := store.UpsertWorkItem(ctx, db.Pool, store.WorkItemRecord{
		PlanID:             planID,
		ID:                 "issue-1",
		UUID:               workItemUUID,
		IssueNumbers:       []int{1},
		AcceptanceCriteria: []string{"Done."},
		DependsOn:          []string{},
		Status:             "running", // internal/planning.Running — cycle has no dependency on that package
	}); err != nil {
		t.Fatalf("upsert work item: %v", err)
	}
}

func TestDispatchCompletesOnFirstSuccessfulAttempt(t *testing.T) {
	db := testDB(t)
	planID := store.NewID()
	workItemUUID := store.NewID()
	insertWorkItemForDispatch(t, db, planID, workItemUUID)

	eng := &scriptedEngine{results: []engine.Result{{ExitCode: 0, ValidationPassed: true}}}
	command := workItemReady{CorrelationID: "corr-1", PlanID: planID, WorkItemID: workItemUUID, Repository: "os-santiago/homedir-dispatch-test", BaseRef: "main", IssueNumbers: []int{1}, AcceptanceCriteria: []string{"Done."}}

	decision, attemptNumber, err := dispatch(context.Background(), db, eng, command, DefaultPolicy())
	if err != nil {
		t.Fatalf("dispatch() error = %v", err)
	}
	if decision.Outcome != Completed {
		t.Fatalf("decision.Outcome = %q, want %q", decision.Outcome, Completed)
	}
	if attemptNumber != 1 {
		t.Fatalf("attemptNumber = %d, want 1", attemptNumber)
	}
	if eng.calls != 1 {
		t.Fatalf("engine was invoked %d times, want 1", eng.calls)
	}

	count, err := store.CountAttempts(context.Background(), db.Pool, workItemUUID)
	if err != nil {
		t.Fatalf("CountAttempts: %v", err)
	}
	if count != 1 {
		t.Fatalf("persisted attempt count = %d, want 1", count)
	}
}

func TestDispatchRetriesThenEscalatesAtAttemptCeiling(t *testing.T) {
	db := testDB(t)
	planID := store.NewID()
	workItemUUID := store.NewID()
	insertWorkItemForDispatch(t, db, planID, workItemUUID)

	// Every attempt fails (non-zero exit); with MaximumAttempts: 2, the
	// first retries (Continue) and the second escalates.
	eng := &scriptedEngine{results: []engine.Result{{ExitCode: 1}, {ExitCode: 1}}}
	command := workItemReady{CorrelationID: "corr-1", PlanID: planID, WorkItemID: workItemUUID, Repository: "os-santiago/homedir-dispatch-test", BaseRef: "main", IssueNumbers: []int{1}, AcceptanceCriteria: []string{"Done."}}
	policy := Policy{MaximumAttempts: 2, MaximumChangedFiles: 0}

	decision, attemptNumber, err := dispatch(context.Background(), db, eng, command, policy)
	if err != nil {
		t.Fatalf("dispatch() error = %v", err)
	}
	if decision.Outcome != Escalated {
		t.Fatalf("decision.Outcome = %q, want %q", decision.Outcome, Escalated)
	}
	if attemptNumber != 2 {
		t.Fatalf("attemptNumber = %d, want 2", attemptNumber)
	}
	if eng.calls != 2 {
		t.Fatalf("engine was invoked %d times, want 2", eng.calls)
	}
}

func TestDispatchResumesFromPriorAttemptCount(t *testing.T) {
	db := testDB(t)
	planID := store.NewID()
	workItemUUID := store.NewID()
	insertWorkItemForDispatch(t, db, planID, workItemUUID)

	ctx := context.Background()
	if err := store.InsertAttempt(ctx, db.Pool, store.AttemptRecord{WorkItemUUID: workItemUUID, AttemptNumber: 1, Outcome: string(Continue), Reason: "prior attempt"}); err != nil {
		t.Fatalf("seed prior attempt: %v", err)
	}

	eng := &scriptedEngine{results: []engine.Result{{ExitCode: 0, ValidationPassed: true}}}
	command := workItemReady{CorrelationID: "corr-1", PlanID: planID, WorkItemID: workItemUUID, Repository: "os-santiago/homedir-dispatch-test", BaseRef: "main", IssueNumbers: []int{1}, AcceptanceCriteria: []string{"Done."}}

	_, attemptNumber, err := dispatch(ctx, db, eng, command, DefaultPolicy())
	if err != nil {
		t.Fatalf("dispatch() error = %v", err)
	}
	if attemptNumber != 2 {
		t.Fatalf("attemptNumber = %d, want 2 (resumed after 1 prior attempt)", attemptNumber)
	}
}
