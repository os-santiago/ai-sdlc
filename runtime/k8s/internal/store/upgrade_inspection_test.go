package store

import (
	"context"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func upgradeFixture(t *testing.T, omitted int) (*DB, string, []MigrationIdentity) {
	t.Helper()
	base := testDB(t)
	ctx := context.Background()
	name := "upgrade_" + strings.ReplaceAll(NewID(), "-", "")
	if _, err := base.Pool.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = base.Pool.ExecContext(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)") })
	u, err := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	q := u.Query()
	q.Del("dbname")
	q.Del("database")
	q.Del("search_path")
	u.RawQuery = q.Encode()
	db, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var actual string
	if err = db.Pool.QueryRowContext(ctx, `SELECT current_database()`).Scan(&actual); err != nil || actual != name {
		t.Fatal("upgrade fixture did not select its isolated database")
	}
	catalog, err := MigrationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	source := slices.Clone(catalog[:len(catalog)-omitted])
	if _, err = db.Pool.ExecContext(ctx, `CREATE TABLE public.schema_migrations(filename text PRIMARY KEY,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, item := range source {
		raw, err := migrationsFS.ReadFile("migrations/" + item.Filename)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := db.Pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, string(raw)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO public.schema_migrations(filename) VALUES($1)`, item.Filename); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	plan := NewID()
	if _, err = db.Pool.ExecContext(ctx, `INSERT INTO public.plans(id,repository,correlation_id) VALUES($1,'example/app','upgrade-fixture')`, plan); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"ready", "completed"} {
		if _, err = db.Pool.ExecContext(ctx, `INSERT INTO public.work_items(plan_id,id,uuid,issue_numbers,acceptance_criteria,depends_on,status)
   VALUES($1,$2,$3,'[17]','["retain original work"]','[]',$2)`, plan, status, NewID()); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Pool.ExecContext(ctx, `INSERT INTO public.outbox(id,aggregate_type,aggregate_id,event_type,payload,correlation_id,published_at)
   VALUES($1,'fixture',$2,'retained','{"nested":{"expected":"unchanged"}}','upgrade-fixture',CASE WHEN $2='completed' THEN now() ELSE NULL END)`, NewID(), status); err != nil {
			t.Fatal(err)
		}
	}
	return db, name, source
}

func upgradeHistory(t *testing.T, db *DB) string {
	t.Helper()
	var result string
	err := db.Pool.QueryRowContext(context.Background(), `SELECT jsonb_build_object(
  'plans',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM public.plans p),
  'work',(SELECT jsonb_agg(to_jsonb(w) ORDER BY uuid) FROM public.work_items w),
  'outbox',(SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM public.outbox o))::text`).Scan(&result)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertUpgradeReport(t *testing.T, r UpgradeMigrationObservation, state string) {
	t.Helper()
	if r.State != state || r.ObservedAt.IsZero() || r.HistoricalSQLHashesVerified || r.SchemaIntegrityVerified || r.ProducerExclusionVerified || r.UpgradeAuthorized || r.RollbackAuthorized || r.WorkflowEvidence {
		t.Fatalf("unexpected observation or authority: %+v", r)
	}
}

func TestUpgradeObservationPreservesPendingAndCompletedHistory(t *testing.T) {
	db, name, source := upgradeFixture(t, 1)
	ctx := context.Background()
	before := upgradeHistory(t, db)
	report, err := db.InspectUpgradeMigrations(ctx, name, source)
	if err != nil {
		t.Fatal(err)
	}
	assertUpgradeReport(t, report, "source-recorded")
	if len(report.RemainingMigrations) != 1 || upgradeHistory(t, db) != before {
		t.Fatal("observation changed history or missed pending migration")
	}
	// Real N-1 -> N schema application in a disposable database, never the pilot.
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	report, err = db.InspectUpgradeMigrations(ctx, name, source)
	if err != nil {
		t.Fatal(err)
	}
	assertUpgradeReport(t, report, "target-recorded")
	if len(report.RemainingMigrations) != 0 || upgradeHistory(t, db) != before {
		t.Fatal("schema upgrade changed durable work")
	}
	if err = db.Migrate(ctx); err != nil || upgradeHistory(t, db) != before {
		t.Fatal("idempotent migration changed history", err)
	}
}

func TestUpgradeObservationDetectsPartialMigrationWithoutClaimingRollback(t *testing.T) {
	db, name, source := upgradeFixture(t, 2)
	ctx := context.Background()
	before := upgradeHistory(t, db)
	catalog, _ := MigrationCatalog()
	last := catalog[len(catalog)-1].Filename
	if last != "0036_review_feedback.sql" {
		t.Fatal("Update the explicit final-file rollback assertions for the new migration")
	}
	// First pending file commits; fail recording the second file so its DDL and
	// tracker insert roll back together. The first file must remain recorded.
	statement := `CREATE FUNCTION public.reject_upgrade_record() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN
  IF NEW.filename='` + last + `' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END$$;
  CREATE TRIGGER upgrade_failure BEFORE INSERT ON public.schema_migrations FOR EACH ROW EXECUTE FUNCTION public.reject_upgrade_record()`
	if _, err := db.Pool.ExecContext(ctx, statement); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err == nil {
		t.Fatal("injected migration failure not reached")
	}
	report, err := db.InspectUpgradeMigrations(ctx, name, source)
	if err != nil {
		t.Fatal(err)
	}
	assertUpgradeReport(t, report, "partial-migration-recorded")
	if len(report.RecordedMigrations) != len(source)+1 || len(report.RemainingMigrations) != 1 || report.RemainingMigrations[0].Filename != last || upgradeHistory(t, db) != before {
		t.Fatal("partial migration lost committed prefix or work")
	}
	var finalTableAbsent bool
	if err = db.Pool.QueryRowContext(ctx, `SELECT to_regclass('public.execution_failures') IS NOT NULL
 AND to_regclass('public.deployment_browser_intents') IS NOT NULL
 AND to_regclass('public.deployment_browser_sends') IS NOT NULL
 AND to_regclass('public.deployment_browser_pods') IS NOT NULL
 AND to_regclass('public.deployment_browser_evidence') IS NOT NULL
 AND to_regclass('public.deployment_browser_cleanups') IS NOT NULL
 AND to_regclass('public.review_feedback') IS NULL`).Scan(&finalTableAbsent); err != nil || !finalTableAbsent {
		t.Fatal("failed file DDL did not roll back", err)
	}
	if _, err = db.Pool.ExecContext(ctx, `DROP TRIGGER upgrade_failure ON public.schema_migrations; DROP FUNCTION public.reject_upgrade_record()`); err != nil {
		t.Fatal(err)
	}
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	report, err = db.InspectUpgradeMigrations(ctx, name, source)
	if err != nil {
		t.Fatal(err)
	}
	assertUpgradeReport(t, report, "target-recorded")
	if upgradeHistory(t, db) != before {
		t.Fatal("fixture recovery changed retained work")
	}
}

func TestUpgradeObservationRejectsTrackingDriftAndBoundedWait(t *testing.T) {
	db, name, source := upgradeFixture(t, 1)
	ctx := context.Background()
	before := upgradeHistory(t, db)
	if _, err := db.InspectUpgradeMigrations(ctx, name+"_wrong", source); err == nil {
		t.Fatal("wrong database accepted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := db.InspectUpgradeMigrations(canceled, name, source); err == nil {
		t.Fatal("canceled observation accepted")
	}
	tx, err := db.Pool.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`LOCK TABLE public.schema_migrations IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err = db.InspectUpgradeMigrations(ctx, name, source); err == nil {
		t.Fatal("blocked observation succeeded")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("observation wait unbounded")
	}
	_ = tx.Rollback()
	for _, statement := range []string{`DELETE FROM public.schema_migrations WHERE filename='0001_init.sql'`,
		`UPDATE public.schema_migrations SET filename='9999_foreign.sql' WHERE filename='0001_init.sql'`,
		`ALTER TABLE public.schema_migrations RENAME TO retained_tracker; CREATE VIEW public.schema_migrations AS SELECT * FROM public.retained_tracker`} {
		// Only this disposable fixture is mutated. Restore its tracker after each
		// denied observation; work rows remain byte-for-byte identical throughout.
		var restore string
		if strings.HasPrefix(statement, "ALTER") {
			restore = `DROP VIEW public.schema_migrations; ALTER TABLE public.retained_tracker RENAME TO schema_migrations`
		} else if strings.HasPrefix(statement, "UPDATE") {
			restore = `UPDATE public.schema_migrations SET filename='0001_init.sql' WHERE filename='9999_foreign.sql'`
		} else {
			restore = `INSERT INTO public.schema_migrations(filename) VALUES('0001_init.sql')`
		}
		if _, err = db.Pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
		if _, err = db.InspectUpgradeMigrations(ctx, name, source); err == nil {
			t.Fatal("tracking drift accepted")
		}
		if _, err = db.Pool.ExecContext(ctx, restore); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.InspectUpgradeMigrations(ctx, name, source); err != nil || upgradeHistory(t, db) != before {
		t.Fatal("fixture did not recover or work changed", err)
	}
}

func TestUpgradeMigrationSourceRequiresExactCompiledPrefix(t *testing.T) {
	catalog, err := MigrationCatalog()
	if err != nil || len(catalog) < 3 {
		t.Fatal(err)
	}
	if ValidateMigrationSource(catalog) != nil || ValidateMigrationSource(catalog[:len(catalog)-1]) != nil {
		t.Fatal("valid compiled prefix rejected")
	}
	changed := slices.Clone(catalog)
	changed[0].SHA256 = strings.Repeat("0", 64)
	reordered := slices.Clone(catalog)
	reordered[0], reordered[1] = reordered[1], reordered[0]
	for _, source := range [][]MigrationIdentity{nil, {}, changed, reordered, catalog[1:], append(slices.Clone(catalog), catalog[0])} {
		if ValidateMigrationSource(source) == nil {
			t.Fatal("noncanonical source accepted")
		}
	}
}

func TestUpgradeObservationRejectsFilteredRole(t *testing.T) {
	db, name, source := upgradeFixture(t, 1)
	ctx := context.Background()
	role := "upgrade_reader_" + strings.ReplaceAll(NewID(), "-", "")
	db.Pool.SetMaxOpenConns(1)
	if _, err := db.Pool.ExecContext(ctx, "CREATE ROLE "+role+" NOSUPERUSER NOBYPASSRLS"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.ExecContext(ctx, "RESET ROLE")
		_, _ = db.Pool.ExecContext(ctx, "DROP OWNED BY "+role)
		_, _ = db.Pool.ExecContext(ctx, "DROP ROLE "+role)
	})
	if _, err := db.Pool.ExecContext(ctx, "GRANT SELECT ON public.schema_migrations TO "+role); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.ExecContext(ctx, "SET ROLE "+role); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InspectUpgradeMigrations(ctx, name, source); err == nil {
		t.Fatal("non-bypass role accepted")
	}
	if _, err := db.Pool.ExecContext(ctx, "RESET ROLE"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InspectUpgradeMigrations(ctx, name, source); err != nil {
		t.Fatal("owner positive control failed", err)
	}
}

func TestUpgradeObservationRejectsTrackerReplacementDuringWait(t *testing.T) {
	db, name, source := upgradeFixture(t, 1)
	ctx := context.Background()
	before := upgradeHistory(t, db)
	holder, err := db.Pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	if _, err = holder.ExecContext(ctx, `LOCK TABLE public.schema_migrations IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := db.InspectUpgradeMigrations(ctx, name, source); result <- err }()
	deadline := time.Now().Add(750 * time.Millisecond)
	for {
		var waiting bool
		if err = db.Pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock' AND query LIKE 'LOCK TABLE public.schema_migrations%')`, name).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("inspector did not reach original relation lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err = holder.ExecContext(ctx, `ALTER TABLE public.schema_migrations RENAME TO retained_tracker; CREATE VIEW public.schema_migrations AS SELECT * FROM public.retained_tracker`); err != nil {
		t.Fatal(err)
	}
	if err = holder.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		if err == nil {
			t.Fatal("replacement view was validated against an old snapshot")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("replacement observation did not finish")
	}
	if _, err = db.Pool.ExecContext(ctx, `DROP VIEW public.schema_migrations; ALTER TABLE public.retained_tracker RENAME TO schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.InspectUpgradeMigrations(ctx, name, source); err != nil || upgradeHistory(t, db) != before {
		t.Fatal("restored tracker or retained work invalid", err)
	}
}
