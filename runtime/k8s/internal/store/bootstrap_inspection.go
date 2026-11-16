package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"slices"
	"time"
)

var ErrBootstrapInspection = errors.New("initial installation requires an empty, current primary database")

// This is deliberately a closed inventory. Adding a durable aggregate requires
// reviewing bootstrap exclusion; unknown tables must never be silently ignored.
var bootstrapTables = []string{
	"admission_heads", "admissions", "attempts", "authorized_inbox", "authorized_nonces", "authorized_outbox",
	"build_cleanups", "build_contexts", "build_effects", "build_handoffs", "build_observations", "build_plans", "build_requests", "build_slots", "built_records",
	"completion_handoffs",
	"terminal_requests", "terminal_intents", "terminal_sends", "terminal_receipts",
	"deployment_handoffs", "deployment_health", "deployment_intents", "deployment_receipts", "deployment_requests", "deployment_sends",
	"execution_candidates", "execution_effects", "execution_lifecycles", "execution_models", "execution_requests",
	"intake_cursors", "intake_heads", "intake_observations", "intake_revisions",
	"managed_component_roles", "merge_effects", "merge_requests", "merged_records", "model_allocations", "model_budgets", "model_calls",
	"outbox", "planning_heads", "plans", "promotion_handoffs", "promotion_records", "promotion_requests", "provenance_handoffs", "provenance_records", "provenance_requests",
	"publication_effects", "publication_handoffs", "publication_requests", "published_records", "release_gates",
	"review_effects", "review_model_calls", "review_runs", "schema_migrations", "verification_effects", "verification_handoffs", "verification_requests", "work_items",
}

// InspectEmptyBootstrap observes one snapshot without changing schema, roles or
// workflow records. It does NOT exclude concurrent producers. The installer must
// independently hold its installation lock, keep current policies frozen, stop
// producers and exclude execution workloads throughout its subsequent effects.
// A successful observation is not delivery authority or an upgrade permit.
func (db *DB) InspectEmptyBootstrap(ctx context.Context, database string) error {
	if db.BootstrapInspectionBoundary(ctx, database) != "" {
		return ErrBootstrapInspection
	}
	return nil
}

// BootstrapInspectionBoundary reports only finite, compiled installation
// boundaries. Never expose driver errors, connection data or database contents.
// Empty means the same read-only inspection succeeded, not delivery authority.
func (db *DB) BootstrapInspectionBoundary(ctx context.Context, database string) string {
	boundary := "input"
	if db.inspectEmptyBootstrap(ctx, database, &boundary) == nil {
		return ""
	}
	return boundary
}

func (db *DB) inspectEmptyBootstrap(ctx context.Context, database string, boundary *string) error {
	if database == "" || len(database) > 63 || db == nil || db.Pool == nil {
		return ErrBootstrapInspection
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	*boundary = "connection"
	conn, err := db.Pool.Conn(ctx)
	if err != nil {
		return ErrBootstrapInspection
	}
	defer conn.Close()
	*boundary = "transaction"
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable, ReadOnly: true})
	if err != nil {
		return ErrBootstrapInspection
	}
	defer tx.Rollback()
	*boundary = "settings"
	for _, statement := range []string{
		`SET LOCAL statement_timeout = '3000ms'`,
		`SET LOCAL lock_timeout = '1000ms'`,
		`SET LOCAL search_path = pg_catalog`,
		`SET LOCAL row_security = off`,
	} {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return ErrBootstrapInspection
		}
	}
	var valid bool
	*boundary = "primary-authority"
	if err = tx.QueryRowContext(ctx, `SELECT current_database() = $1 AND NOT pg_is_in_recovery()
		AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = current_user AND (rolsuper OR rolbypassrls))`, database).Scan(&valid); err != nil || !valid {
		return ErrBootstrapInspection
	}
	// Supported runtime migrations create relations only in public. Reject
	// unknown application schemas instead of silently excluding archived work.
	*boundary = "foreign-schema"
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname <> 'public' AND n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'
		AND c.relkind IN ('r','p','v','m','f'))`).Scan(&valid); err != nil || valid {
		return ErrBootstrapInspection
	}
	*boundary = "table-inventory"
	rows, err := tx.QueryContext(ctx, `SELECT c.relname, c.relkind::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND c.relkind IN ('r','p','v','m','f') ORDER BY c.relname`)
	if err != nil {
		return ErrBootstrapInspection
	}
	var actual []string
	for rows.Next() {
		var name, kind string
		if rows.Scan(&name, &kind) != nil || kind != "r" {
			rows.Close()
			return ErrBootstrapInspection
		}
		actual = append(actual, name)
	}
	readErr := rows.Err()
	rows.Close()
	if readErr != nil || !slices.Equal(actual, bootstrapTables) {
		return ErrBootstrapInspection
	}
	*boundary = "migration-inventory"
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return ErrBootstrapInspection
	}
	var count int
	if tx.QueryRowContext(ctx, `SELECT count(*) FROM public.schema_migrations`).Scan(&count) != nil || count != len(entries) {
		return ErrBootstrapInspection
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return ErrBootstrapInspection
		}
		if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.schema_migrations WHERE filename=$1)`, entry.Name()).Scan(&valid) != nil || !valid {
			return ErrBootstrapInspection
		}
	}
	for _, name := range bootstrapTables {
		if name == "schema_migrations" || name == "managed_component_roles" {
			continue
		}
		// Identifiers come exclusively from the compiled closed inventory above.
		*boundary = "history/" + name
		if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.`+name+`)`).Scan(&valid) != nil || valid {
			return ErrBootstrapInspection
		}
	}
	*boundary = "commit"
	if tx.Commit() != nil {
		return ErrBootstrapInspection
	}
	return nil
}
