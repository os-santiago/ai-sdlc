package store

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestConcurrentFreshMigrations(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database := "startup_" + strings.ReplaceAll(NewID(), "-", "")
	if _, err := db.Pool.ExecContext(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	defer db.Pool.ExecContext(context.Background(), "DROP DATABASE "+database+" WITH (FORCE)")
	u, err := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Del("search_path")
	q.Del("dbname")
	q.Del("database")
	u.Path = "/" + database
	u.RawQuery = q.Encode()
	results := make(chan error, 5)
	for i := 0; i < 5; i++ {
		go func() {
			other, err := Open(ctx, u.String())
			if err == nil {
				defer other.Close()
				err = other.Migrate(ctx)
			}
			results <- err
		}()
	}
	for i := 0; i < 5; i++ {
		if err := <-results; err != nil {
			t.Errorf("concurrent startup: %v", err)
		}
	}
}

func TestMigrateRejectsForeignSchemaBeforeEffects(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	schema := "migration_scope_" + strings.ReplaceAll(NewID(), "-", "")
	if _, err := db.Pool.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer db.Pool.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	u, err := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	other, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.Migrate(ctx); err == nil || err.Error() != "runtime migrations require the dedicated public schema" {
		t.Fatal("foreign schema migration did not fail closed", err)
	}
	var count int
	if err := db.Pool.QueryRowContext(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1`, schema).Scan(&count); err != nil || count != 0 {
		t.Fatal("foreign schema gained migration objects", err)
	}
}

// testDB opens a real Postgres connection and runs migrations, skipping the
// test when AI_SDLC_TEST_DATABASE_URL is unset. This runtime's design goal
// (ADR-0004 README: "No external services required ... every test is a
// pure unit test") still holds for admission/cycle/planning/gates; the
// store/outbox integration tests are the deliberate, narrowly-scoped
// exception, and they skip cleanly in any environment without a database
// (including CI, until a Postgres service container is wired in — tracked
// as follow-up, not required for this package to build/vet/unit-test).
func testDB(t *testing.T) *DB {
	t.Helper()
	url := os.Getenv("AI_SDLC_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AI_SDLC_TEST_DATABASE_URL not set; skipping Postgres-backed test")
	}
	db, err := Open(context.Background(), url)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return db
}

func TestNewIDLooksLikeUUIDv4(t *testing.T) {
	id := NewID()
	if len(id) != 36 {
		t.Fatalf("NewID() = %q, want length 36, got %d", id, len(id))
	}
	if id[14] != '4' {
		t.Fatalf("NewID() = %q, want version nibble '4' at index 14", id)
	}
	variant := id[19]
	if variant != '8' && variant != '9' && variant != 'a' && variant != 'b' {
		t.Fatalf("NewID() = %q, want RFC 4122 variant nibble at index 19, got %q", id, variant)
	}
}

func TestNewIDIsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := NewID()
		if seen[id] {
			t.Fatalf("NewID() produced a duplicate: %q", id)
		}
		seen[id] = true
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate() call: %v", err)
	}
}

func TestUpsertAdmissionIsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rec := AdmissionRecord{
		Repository:    "os-santiago/homedir",
		IssueNumber:   1573,
		IssueRevision: "rev-1",
		Decision:      "accepted",
		Risk:          "low",
		PolicyVersion: "1.0.0",
		Reasons:       []string{"No blocking admission risks detected."},
		RequiredGates: []string{"tests"},
		CorrelationID: "os-santiago/homedir#1573@rev-1",
	}
	if err := UpsertAdmission(ctx, db.Pool, rec); err != nil {
		t.Fatalf("first UpsertAdmission: %v", err)
	}
	rec.Decision = "needs-human"
	if err := UpsertAdmission(ctx, db.Pool, rec); err != nil {
		t.Fatalf("second UpsertAdmission (re-delivery): %v", err)
	}

	var decision string
	row := db.Pool.QueryRowContext(ctx, `SELECT decision FROM admissions WHERE repository = $1 AND issue_number = $2 AND issue_revision = $3`,
		rec.Repository, rec.IssueNumber, rec.IssueRevision)
	if err := row.Scan(&decision); err != nil {
		t.Fatalf("read back admission: %v", err)
	}
	if decision != "needs-human" {
		t.Fatalf("decision = %q, want %q (re-delivery should update, not duplicate)", decision, "needs-human")
	}
}
