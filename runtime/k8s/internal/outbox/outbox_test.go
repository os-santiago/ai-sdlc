package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

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

func TestDeliverUsesConfiguredRoute(t *testing.T) {
	var receivedBody []byte
	var receivedIdempotencyKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedIdempotencyKey = r.Header.Get("Idempotency-Key")
		receivedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	relay := &Relay{Routes: map[string]string{"Test.v1": server.URL}}
	row := outboxRow{ID: "row-1", EventType: "Test.v1", Payload: json.RawMessage(`{"ok":true}`), CorrelationID: "corr-1"}
	if err := relay.deliver(context.Background(), row); err != nil {
		t.Fatalf("deliver() error = %v", err)
	}
	if receivedIdempotencyKey != "row-1" {
		t.Fatalf("Idempotency-Key header = %q, want %q", receivedIdempotencyKey, "row-1")
	}
	if string(receivedBody) != `{"ok":true}` {
		t.Fatalf("request body = %q, want %q", receivedBody, `{"ok":true}`)
	}
}

func TestDeliverFailsOnUnknownEventType(t *testing.T) {
	relay := &Relay{Routes: map[string]string{}}
	row := outboxRow{ID: "row-1", EventType: "Unrouted.v1", Payload: json.RawMessage(`{}`)}
	if err := relay.deliver(context.Background(), row); err == nil {
		t.Fatal("deliver() with no route configured, want error")
	}
}

func TestDeliverFailsOnNon2xxStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	relay := &Relay{Routes: map[string]string{"Test.v1": server.URL}}
	row := outboxRow{ID: "row-1", EventType: "Test.v1", Payload: json.RawMessage(`{}`)}
	if err := relay.deliver(context.Background(), row); err == nil {
		t.Fatal("deliver() with 500 response, want error")
	}
}

func TestAppendAndPollOnceDeliversAndMarksPublished(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	var delivered int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := db.WithTx(ctx, func(tx *sql.Tx) error {
		return Append(ctx, tx, Record{AggregateType: "test", AggregateID: "1", EventType: "Test.v1", Payload: map[string]bool{"ok": true}, CorrelationID: "corr-1"})
	}); err != nil {
		t.Fatalf("Append within WithTx: %v", err)
	}

	relay := &Relay{DB: db, Routes: map[string]string{"Test.v1": server.URL}}
	n, err := relay.PollOnce(ctx)
	if err != nil {
		t.Fatalf("PollOnce() error = %v", err)
	}
	if n != 1 {
		t.Fatalf("PollOnce() delivered = %d, want 1", n)
	}
	if delivered != 1 {
		t.Fatalf("server received %d requests, want 1", delivered)
	}

	// A second poll must find nothing left to deliver.
	n, err = relay.PollOnce(ctx)
	if err != nil {
		t.Fatalf("second PollOnce() error = %v", err)
	}
	if n != 0 {
		t.Fatalf("second PollOnce() delivered = %d, want 0 (row already published)", n)
	}
}
