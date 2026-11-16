package store

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// A new database per case prevents existing integration-test history from
// masking positive controls. These operations affect disposable fixtures only.
func bootstrapInspectionDB(t *testing.T) (*DB, string, string) {
	t.Helper()
	base := testDB(t)
	name := "bootstrap_" + strings.ReplaceAll(NewID(), "-", "")
	if _, err := base.Pool.ExecContext(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = base.Pool.ExecContext(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)") })
	u, err := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	query := u.Query()
	query.Del("dbname")
	query.Del("database")
	u.RawQuery = query.Encode()
	db, err := Open(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var actual string
	if err = db.Pool.QueryRowContext(context.Background(), `SELECT current_database()`).Scan(&actual); err != nil || actual != name {
		t.Fatal("disposable database was not selected")
	}
	if err = db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db, name, u.String()
}

func TestBootstrapInspectionEmptyAndCompletedHistory(t *testing.T) {
	db, name, _ := bootstrapInspectionDB(t)
	ctx := context.Background()
	if err := db.InspectEmptyBootstrap(ctx, name); err != nil {
		t.Fatal(err)
	}
	if boundary := db.BootstrapInspectionBoundary(ctx, name); boundary != "" {
		t.Fatalf("empty fixture rejected at compiled boundary %s", boundary)
	}
	// Even successfully delivered historical work disqualifies first install.
	if _, err := db.Pool.ExecContext(ctx, `INSERT INTO public.outbox(id,aggregate_type,aggregate_id,event_type,payload,correlation_id,published_at)
		VALUES($1,'fixture','fixture','completed','{}','fixture',now())`, NewID()); err != nil {
		t.Fatal(err)
	}
	if err := db.InspectEmptyBootstrap(ctx, name); err != ErrBootstrapInspection {
		t.Fatalf("history accepted: %v", err)
	}
	if boundary := db.BootstrapInspectionBoundary(ctx, name); boundary != "history/outbox" {
		t.Fatalf("unexpected compiled rejection boundary %s", boundary)
	}
	var count int
	if err := db.Pool.QueryRowContext(ctx, `SELECT count(*) FROM public.outbox`).Scan(&count); err != nil || count != 1 {
		t.Fatal("inspection changed history", err)
	}
}

func TestBootstrapFixtureRejectsDatabaseQueryOverride(t *testing.T) {
	base := testDB(t)
	var original string
	if err := base.Pool.QueryRowContext(context.Background(), `SELECT current_database()`).Scan(&original); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"dbname", "database"} {
		t.Run(alias, func(t *testing.T) {
			u, err := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			q.Set(alias, original)
			u.RawQuery = q.Encode()
			t.Setenv("AI_SDLC_TEST_DATABASE_URL", u.String())
			db, name, _ := bootstrapInspectionDB(t)
			if name == original || db.InspectEmptyBootstrap(context.Background(), name) != nil {
				t.Fatal("fixture did not isolate its database")
			}
		})
	}
}

func TestBootstrapInspectionRejectsMissingOrUnknownSchema(t *testing.T) {
	for _, change := range []string{
		`ALTER TABLE public.outbox RENAME TO unexpected_outbox`,
		`CREATE TABLE public.unreviewed_work(id integer)`,
		`DELETE FROM public.schema_migrations WHERE filename='0026_provenance_handoff.sql'`,
		`ALTER TABLE public.outbox SET SCHEMA pg_catalog`,
		`CREATE SCHEMA archive; CREATE TABLE archive.archived_work(id integer); INSERT INTO archive.archived_work VALUES (1)`,
	} {
		t.Run(change, func(t *testing.T) {
			db, name, _ := bootstrapInspectionDB(t)
			if _, err := db.Pool.ExecContext(context.Background(), change); err != nil {
				t.Fatal(err)
			}
			if err := db.InspectEmptyBootstrap(context.Background(), name); err != ErrBootstrapInspection {
				t.Fatalf("schema accepted: %v", err)
			}
		})
	}
}

func TestBootstrapInspectionRejectsWrongDatabaseAndDeadline(t *testing.T) {
	db, name, _ := bootstrapInspectionDB(t)
	if err := db.InspectEmptyBootstrap(context.Background(), name+"_wrong"); err != ErrBootstrapInspection {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := db.InspectEmptyBootstrap(ctx, name); err != ErrBootstrapInspection {
		t.Fatal(err)
	}
	tx, err := db.Pool.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`LOCK TABLE public.outbox IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err = db.InspectEmptyBootstrap(context.Background(), name); err != ErrBootstrapInspection {
		t.Fatal(err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("lock timeout was not bounded")
	}
	_ = tx.Rollback()
	if err = db.InspectEmptyBootstrap(context.Background(), name); err != nil {
		t.Fatal("connection did not recover", err)
	}
}

func TestBootstrapInspectionRejectsRLSFilteredReader(t *testing.T) {
	db, name, uri := bootstrapInspectionDB(t)
	ctx := context.Background()
	role := "bootstrap_reader_" + strings.ReplaceAll(NewID(), "-", "")
	if _, err := db.Pool.ExecContext(ctx, "CREATE ROLE "+role); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.ExecContext(ctx, "DROP OWNED BY "+role)
		_, _ = db.Pool.ExecContext(ctx, "DROP ROLE "+role)
	})
	for _, q := range []string{
		"GRANT USAGE ON SCHEMA public TO " + role,
		"GRANT SELECT ON ALL TABLES IN SCHEMA public TO " + role,
		`ALTER TABLE public.outbox ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE public.outbox FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY deny_all ON public.outbox USING (false)`,
	} {
		if _, err := db.Pool.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool.ExecContext(ctx, `INSERT INTO public.outbox VALUES($1,'fixture','fixture','completed','{}','fixture',now(),now())`, NewID()); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(uri)
	q := u.Query()
	q.Set("options", "-crole="+role)
	u.RawQuery = q.Encode()
	reader, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var count int
	if err = reader.Pool.QueryRowContext(ctx, `SELECT count(*) FROM public.outbox`).Scan(&count); err != nil || count != 0 {
		t.Fatal("RLS negative control invalid", err, count)
	}
	if err = reader.InspectEmptyBootstrap(ctx, name); err != ErrBootstrapInspection {
		t.Fatal("RLS reader accepted", err)
	}
	if err = db.InspectEmptyBootstrap(ctx, name); err != ErrBootstrapInspection {
		t.Fatal("privileged reader missed hidden history", err)
	}
}
