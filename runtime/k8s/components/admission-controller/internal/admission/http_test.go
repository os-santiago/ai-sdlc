package admission

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

func validRequestBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(evaluateRequest{
		Issue: Issue{
			Repository:         "os-santiago/homedir",
			Number:             1573,
			Revision:           "rev-1",
			Title:              "Fix the thing that is broken",
			Body:               "This issue body is long enough to satisfy the minimum body length policy for admission evaluation purposes in this unit test scenario.",
			AcceptanceCriteria: []string{"The thing is fixed."},
			RequestedBy:        "octocat",
			AuthorizedBy:       "octocat",
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return body
}

func TestRoutesWithNilDBBehavesLikeADR0004(t *testing.T) {
	mux := http.NewServeMux()
	Routes(mux, Deps{})

	req := httptest.NewRequest(http.MethodPost, "/v1/admissions/evaluate", bytes.NewReader(validRequestBody(t)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var result Result
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result.Decision != Accepted {
		t.Fatalf("decision = %q, want %q; reasons: %v", result.Decision, Accepted, result.Reasons)
	}
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

func TestRoutesWithDBPersistsAndEmitsOutboxEvent(t *testing.T) {
	db := testDB(t)
	mux := http.NewServeMux()
	Routes(mux, Deps{DB: db})

	req := httptest.NewRequest(http.MethodPost, "/v1/admissions/evaluate", bytes.NewReader(validRequestBody(t)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var decision string
	row := db.Pool.QueryRowContext(context.Background(),
		`SELECT decision FROM admissions WHERE repository = $1 AND issue_number = $2 AND issue_revision = $3`,
		"os-santiago/homedir", 1573, "rev-1")
	if err := row.Scan(&decision); err != nil {
		t.Fatalf("read back admission: %v", err)
	}
	if decision != "accepted" {
		t.Fatalf("decision = %q, want %q", decision, "accepted")
	}

	var outboxCount int
	row = db.Pool.QueryRowContext(context.Background(),
		`SELECT count(*) FROM outbox WHERE event_type = 'IssueAdmissionDecided.v1' AND aggregate_id = $1`,
		"os-santiago/homedir#1573@rev-1")
	if err := row.Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if outboxCount != 1 {
		t.Fatalf("outbox rows for this admission = %d, want 1", outboxCount)
	}
}
