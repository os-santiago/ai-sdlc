package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var ErrUpgradeFence = errors.New("runtime upgrade fence denied")

// ErrUpgradeFenceHistory is emitted only after a successful SQL observation
// proves the reviewed history predicate false. It never classifies I/O errors.
var ErrUpgradeFenceHistory = fmt.Errorf("%w: history predicate rejected", ErrUpgradeFence)

//go:embed reviewer_upgrade_history.sql
var reviewerUpgradeHistory string

//go:embed review_feedback_upgrade_history.sql
var reviewFeedbackUpgradeHistory string

//go:embed configuration_upgrade_in_flight.sql
var configurationUpgradeInFlight string

// UpgradeFence exists only during its original bounded transaction. It grants
// no delivery, migration or rollback authority. Call Check immediately before
// and after each original-identity-bound administrative Kubernetes operation.
type UpgradeFence struct {
	checkPermit     chan struct{}
	tx              *sql.Tx
	tables          []string
	relationIDs     string
	database        string
	source          []MigrationIdentity
	deadline        time.Time
	reviewerHistory bool
	feedbackHistory bool
	repairHistory   bool
	// configurationHistory admits all completed history of the exact current
	// catalog and rejects only in-flight work (no migration exists).
	configurationHistory bool
}

// This reviewed bridge covers schemas 0033 and 0034, before any candidate or
// downstream external delivery history. Additional schemas/states need their
// own compatibility and recovery proof; no history is deleted to qualify.
var browserUpgradeTables = []string{"deployment_browser_cleanups", "deployment_browser_evidence", "deployment_browser_intents", "deployment_browser_pods", "deployment_browser_sends"}
var repairUpgradeTables = []string{"execution_repairs", "repair_model_budgets", "review_repair_handoffs", "review_repair_stops"}

// RelaxationMigrations only relax constraints or add inert SELECT policies
// for the optional flow console observer: they add no table, column, role,
// grant or row, so a configuration upgrade may apply them on top of the
// catalog it fences (a source missing them is admitted, and the upgrade's
// own migrator job applies them). A policy naming the observer admits
// nothing until role provisioning creates that role and grants its columns.
// 0040 narrows the merge request uniqueness to requests that may have sent
// (a partial unique index replacing the full one) and admits the not-sent
// reason. 0041 widens the executor source and export size checks to the
// larger workspace bundle bound. Every other migration needs its own bridge
// (see ToolJournalMigrations for the one additive table it admits).
var RelaxationMigrations = []string{"0038_generic_publication_scope.sql", "0039_flow_console_reader.sql", "0040_merge_retry_before_send.sql", "0041_large_workspace_bounds.sql"}

// ToolJournalMigrations is NOT a relaxation: 0042 adds the tool_calls table,
// its owner policy and (through role provisioning) worker grants. It is the
// one additive migration the configuration fence also admits as missing, by
// this reviewed argument rather than by the relaxation rule:
//   - the table is created empty and no migration fills it; only a generator
//     tool call of a claimed execution writes a row (BeginToolCall requires
//     the claim), so no historical record gains or loses meaning;
//   - the configuration fence already refuses every claimed or uncleaned
//     execution and every open model budget or call of the source, so no
//     generation that could have wanted a journal row is in flight across
//     the upgrade;
//   - a source without 0042 has no tool_calls table: the fence locks and
//     inventories only the source tables (tool_calls is excluded below for
//     sources older than 42), and the upgrade's own migrator Job creates it.
//
// A source of 42 is the current catalog and fences tool_calls too; a started
// tool call is then in-flight work. A later additive table needs its own
// review: this list is not a general admission rule for new tables.
var ToolJournalMigrations = []string{"0042_generator_tool_calls.sql"}

// configurationSource reports whether source is the current catalog, or the
// current catalog without a suffix made only of relaxation migrations and the
// reviewed tool journal addition.
func configurationSource(source []MigrationIdentity) bool {
	catalog, err := MigrationCatalog()
	if err != nil || ValidateMigrationSource(source) != nil || len(source) < len(catalog)-len(RelaxationMigrations)-len(ToolJournalMigrations) {
		return false
	}
	for _, migration := range catalog[len(source):] {
		if !slices.Contains(RelaxationMigrations, migration.Filename) && !slices.Contains(ToolJournalMigrations, migration.Filename) {
			return false
		}
	}
	return true
}

func upgradeFenceTables(source []MigrationIdentity) ([]string, error) {
	return selectedUpgradeFenceTables(source, false)
}

func selectedUpgradeFenceTables(source []MigrationIdentity, browser bool) ([]string, error) {
	return scopedUpgradeFenceTables(source, browser, false)
}

func scopedUpgradeFenceTables(source []MigrationIdentity, browser, feedback bool) ([]string, error) {
	return repairScopedUpgradeFenceTables(source, browser, feedback, false)
}

func repairScopedUpgradeFenceTables(source []MigrationIdentity, browser, feedback, repair bool) ([]string, error) {
	catalog, err := MigrationCatalog()
	if err != nil || len(catalog) != 42 || catalog[41].Filename != "0042_generator_tool_calls.sql" || catalog[40].Filename != "0041_large_workspace_bounds.sql" || catalog[39].Filename != "0040_merge_retry_before_send.sql" || catalog[38].Filename != "0039_flow_console_reader.sql" || catalog[37].Filename != "0038_generic_publication_scope.sql" || catalog[36].Filename != "0037_review_repair.sql" || catalog[35].Filename != "0036_review_feedback.sql" || catalog[34].Filename != "0035_deployment_browser.sql" || catalog[32].Filename != "0033_production_completion.sql" || catalog[33].Filename != "0034_execution_failures.sql" || ValidateMigrationSource(source) != nil || (browser && feedback) || (repair && (browser || feedback)) {
		return nil, ErrUpgradeFence
	}
	// The repair-scoped selection serves the 36-to-37 repair bridge and the
	// configuration protocol (sources 37 to 42; see configurationSource).
	if (repair && (len(source) < 36 || len(source) > 42)) || (!repair && ((feedback && len(source) != 35 && len(source) != 36) || (!feedback && ((!browser && len(source) != 33 && len(source) != 34) || (browser && len(source) != 34 && len(source) != 35))))) {
		return nil, ErrUpgradeFence
	}
	// Historical bridges never admit the new feedback schema or its rows.
	tables := slices.DeleteFunc(slices.Clone(bootstrapTables), func(s string) bool { return len(source) < 37 && slices.Contains(repairUpgradeTables, s) })
	// Sources older than 0042 have no tool journal: it is never fenced or admitted.
	if len(source) < 42 {
		tables = slices.DeleteFunc(tables, func(s string) bool { return s == "tool_calls" })
	}
	if len(source) < 36 {
		tables = slices.DeleteFunc(tables, func(s string) bool { return s == "review_feedback" })
	}
	if len(source) < 35 {
		tables = slices.DeleteFunc(tables, func(s string) bool { return slices.Contains(browserUpgradeTables, s) })
	}
	if len(source) == 33 {
		tables = slices.DeleteFunc(tables, func(s string) bool { return s == "execution_failures" })
	}
	return tables, nil
}

func upgradeFenceInventory(ctx context.Context, tx *sql.Tx, expected []string) (string, error) {
	var foreign bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname<>'public' AND n.nspname<>'information_schema' AND n.nspname !~ '^pg_' AND c.relkind IN('r','p','v','m','f'))`).Scan(&foreign); err != nil || foreign {
		return "", ErrUpgradeFence
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.relname,c.relkind::text,c.relpersistence::text,c.oid::bigint FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname='public' AND c.relkind IN('r','p','v','m','f') ORDER BY c.relname COLLATE "C" LIMIT 513`)
	if err != nil {
		return "", ErrUpgradeFence
	}
	defer rows.Close()
	var names, ids []string
	for rows.Next() {
		var name, kind, persistence string
		var oid int64
		if rows.Scan(&name, &kind, &persistence, &oid) != nil || kind != "r" || persistence != "p" || oid <= 0 {
			return "", ErrUpgradeFence
		}
		names = append(names, name)
		ids = append(ids, fmt.Sprint(oid))
	}
	if rows.Err() != nil || !slices.Equal(names, expected) {
		return "", ErrUpgradeFence
	}
	return "{" + strings.Join(ids, ",") + "}", nil
}

func (f *UpgradeFence) Check(ctx context.Context) error {
	if f == nil || f.tx == nil || f.checkPermit == nil || time.Now().After(f.deadline) {
		return ErrUpgradeFence
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	// A transaction shares one driver connection. Protect the whole observation,
	// including Rows iteration/Close, from concurrent heartbeat/protocol queries.
	select {
	case f.checkPermit <- struct{}{}:
		defer func() { <-f.checkPermit }()
	case <-ctx.Done():
		return ErrUpgradeFence
	}
	if ctx.Err() != nil || time.Now().After(f.deadline) {
		return ErrUpgradeFence
	}
	var valid bool
	// The same lock used by Migrate also excludes another maintenance fence.
	if err := f.tx.QueryRowContext(ctx, `SELECT current_database()=$1 AND NOT pg_is_in_recovery()
  AND EXISTS(SELECT 1 FROM pg_roles WHERE rolname=current_user AND (rolsuper OR rolbypassrls))
  AND EXISTS(SELECT 1 FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory' AND classid=0 AND objid=73421019 AND objsubid=1 AND mode='ExclusiveLock' AND granted)
  AND (SELECT count(*) FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='relation' AND mode='ShareLock' AND granted AND relation=ANY($2::oid[]))=$3`, f.database, f.relationIDs, len(f.tables)).Scan(&valid); err != nil || !valid {
		return ErrUpgradeFence
	}
	ids, err := upgradeFenceInventory(ctx, f.tx, f.tables)
	if err != nil || ids != f.relationIDs {
		return ErrUpgradeFence
	}
	return nil
}

func (f *UpgradeFence) inspectHistory(ctx context.Context) error {
	rows, err := f.tx.QueryContext(ctx, `SELECT CASE WHEN octet_length(filename)<=128 THEN filename ELSE NULL END FROM public.schema_migrations ORDER BY filename COLLATE "C" LIMIT 513`)
	if err != nil {
		return ErrUpgradeFence
	}
	var names []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			rows.Close()
			return ErrUpgradeFence
		}
		names = append(names, name)
	}
	readErr := rows.Err()
	rows.Close()
	if readErr != nil || len(names) != len(f.source) {
		return ErrUpgradeFence
	}
	for i, name := range names {
		if name != f.source[i].Filename {
			return ErrUpgradeFence
		}
	}
	if f.configurationHistory {
		var blocked bool
		if !configurationSource(f.source) || f.tx.QueryRowContext(ctx, configurationUpgradeInFlight).Scan(&blocked) != nil {
			return ErrUpgradeFence
		}
		if blocked {
			return ErrUpgradeFenceHistory
		}
		// A started tool call is in flight. Only a source that has the
		// journal (42) fences it; older sources cannot hold a row.
		if slices.Contains(f.tables, "tool_calls") {
			if f.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.tool_calls WHERE state='started')`).Scan(&blocked) != nil {
				return ErrUpgradeFence
			}
			if blocked {
				return ErrUpgradeFenceHistory
			}
		}
		return nil
	}
	allowed := map[string]bool{}
	if f.reviewerHistory {
		var safe bool
		query := reviewerUpgradeHistory
		if f.feedbackHistory || f.repairHistory {
			query = reviewFeedbackUpgradeHistory
		}
		if (len(f.source) != 34 && len(f.source) != 35 && len(f.source) != 36 && !(f.repairHistory && len(f.source) == 37)) || f.tx.QueryRowContext(ctx, query).Scan(&safe) != nil {
			return ErrUpgradeFence
		}
		if !safe {
			return ErrUpgradeFenceHistory
		}
		for _, name := range []string{"execution_candidates", "verification_handoffs", "verification_requests", "verification_effects", "review_runs", "review_effects", "review_model_calls"} {
			allowed[name] = true
		}
	}
	for _, name := range []string{"admission_heads", "admissions", "authorized_inbox", "authorized_nonces", "authorized_outbox",
		"execution_effects", "execution_failures", "execution_lifecycles", "execution_models", "execution_requests",
		"intake_cursors", "intake_heads", "intake_observations", "intake_revisions", "managed_component_roles",
		"model_allocations", "model_budgets", "model_calls", "outbox", "planning_heads", "plans", "schema_migrations", "work_items"} {
		allowed[name] = true
	}
	for _, name := range f.tables {
		if allowed[name] {
			continue
		}
		var exists bool
		if err = f.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.`+name+`)`).Scan(&exists); err != nil {
			return ErrUpgradeFence
		}
		if exists {
			return ErrUpgradeFenceHistory
		}
	}
	var blocked bool
	if err = f.tx.QueryRowContext(ctx, `SELECT
  EXISTS(SELECT 1 FROM public.execution_requests WHERE state='claimed')
  OR EXISTS(SELECT 1 FROM public.execution_lifecycles l JOIN public.execution_requests r ON r.id=l.request_id WHERE r.state<>'stopped' OR l.cleanup_state<>'complete')
  OR EXISTS(SELECT 1 FROM public.execution_effects WHERE result IS NULL)
  OR EXISTS(SELECT 1 FROM public.model_budgets WHERE state<>'stopped')
  OR EXISTS(SELECT 1 FROM public.model_calls WHERE state NOT IN('completed','stopped'))`).Scan(&blocked); err != nil {
		return ErrUpgradeFence
	}
	if blocked {
		return ErrUpgradeFenceHistory
	}
	return nil
}

// WithPreCandidateUpgradeFence blocks workflow writes without editing rows.
// SHARE is an administrative READ WRITE transaction, not read-only observation.
// All locks precede the fresh READ COMMITTED history check. If activity or
// unsupported history exists, release immediately so original cleanup can run.
// The callback must verify original Kubernetes identities and absence of live
// execution resources, then pause/confirm original producers before returning.
// Never migrate, await workflow completion, or treat this as a drain receipt.
func (db *DB) WithPreCandidateUpgradeFence(ctx context.Context, database string, source []MigrationIdentity, pause func(context.Context, *UpgradeFence) error) error {
	return db.withUpgradeFence(ctx, database, source, false, false, pause)
}

// WithContainedReviewUpgradeFence is a distinct administrative capability.
// It admits only fully expired, cleaned and failed review history, never
// publication or later work. The original pre-candidate contract is unchanged.
func (db *DB) WithContainedReviewUpgradeFence(ctx context.Context, database string, source []MigrationIdentity, pause func(context.Context, *UpgradeFence) error) error {
	if len(source) != 34 {
		return ErrUpgradeFence
	}
	return db.withUpgradeFence(ctx, database, source, true, false, pause)
}

// WithBrowserDeploymentUpgradeFence is the closed 0034 -> 0035 bridge.
// It requires contained review history and every browser table empty; it never
// grants migration/delivery permission or changes the legacy protocols.
func (db *DB) WithBrowserDeploymentUpgradeFence(ctx context.Context, database string, source []MigrationIdentity, pause func(context.Context, *UpgradeFence) error) error {
	return db.withUpgradeFence(ctx, database, source, true, true, pause)
}

func (db *DB) withUpgradeFence(ctx context.Context, database string, source []MigrationIdentity, reviewerHistory, browser bool, pause func(context.Context, *UpgradeFence) error) error {
	return db.withScopedUpgradeFence(ctx, database, source, reviewerHistory, browser, false, pause)
}

// WithReviewFeedbackUpgradeFence is the separate 0035 -> 0036 administrative
// bridge. Original failed reviews must be expired and cleaned, and every
// browser/feedback/downstream table must be empty. It never repairs work.
func (db *DB) WithReviewFeedbackUpgradeFence(ctx context.Context, database string, source []MigrationIdentity, pause func(context.Context, *UpgradeFence) error) error {
	return db.withScopedUpgradeFence(ctx, database, source, true, false, true, pause)
}

// WithReviewRepairUpgradeFence excludes writers for the exact 36-to-37 bridge.
// Feedback and all child ledgers must be empty: no historical work gains repair.
func (db *DB) WithReviewRepairUpgradeFence(ctx context.Context, database string, source []MigrationIdentity, pause func(context.Context, *UpgradeFence) error) error {
	// The shared table selection also serves the configuration protocol,
	// which admits the 37- to 42-entry catalogs; this bridge admits only 36 and 37.
	if len(source) != 36 && len(source) != 37 {
		return ErrUpgradeFence
	}
	return db.withRepairScopedUpgradeFence(ctx, database, source, true, false, false, true, pause)
}

func (db *DB) withScopedUpgradeFence(ctx context.Context, database string, source []MigrationIdentity, reviewerHistory, browser, feedback bool, pause func(context.Context, *UpgradeFence) error) error {
	return db.withRepairScopedUpgradeFence(ctx, database, source, reviewerHistory, browser, feedback, false, pause)
}

// WithConfigurationUpgradeFence is the no-migration capability for changing
// installation configuration (for example the model gateway adapter) on the
// exact current catalog. Writers are excluded as in every fence, all completed
// history is admitted, and any in-flight model-dependent work is rejected.
// It never grants migration, delivery or repair authority.
func (db *DB) WithConfigurationUpgradeFence(ctx context.Context, database string, source []MigrationIdentity, pause func(context.Context, *UpgradeFence) error) error {
	if !configurationSource(source) {
		return ErrUpgradeFence
	}
	return db.withConfiguredUpgradeFence(ctx, database, source, false, false, false, true, true, pause)
}

func (db *DB) withRepairScopedUpgradeFence(ctx context.Context, database string, source []MigrationIdentity, reviewerHistory, browser, feedback, repair bool, pause func(context.Context, *UpgradeFence) error) error {
	return db.withConfiguredUpgradeFence(ctx, database, source, reviewerHistory, browser, feedback, repair, false, pause)
}

func (db *DB) withConfiguredUpgradeFence(ctx context.Context, database string, source []MigrationIdentity, reviewerHistory, browser, feedback, repair, configuration bool, pause func(context.Context, *UpgradeFence) error) error {
	tables, err := repairScopedUpgradeFenceTables(source, browser, feedback, repair)
	if err != nil || db == nil || db.Pool == nil || database == "" || len(database) > 63 || pause == nil {
		return ErrUpgradeFence
	}
	active, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	tx, err := db.Pool.BeginTx(active, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return ErrUpgradeFence
	}
	defer tx.Rollback()
	for _, statement := range []string{`SET LOCAL statement_timeout='3000ms'`, `SET LOCAL lock_timeout='1000ms'`, `SET LOCAL search_path=pg_catalog`, `SET LOCAL row_security=off`, `SET LOCAL idle_in_transaction_session_timeout='5000ms'`} {
		if _, err = tx.ExecContext(active, statement); err != nil {
			return ErrUpgradeFence
		}
	}
	// READ COMMITTED allows this identity check before any administrative lock,
	// followed by a fresh history snapshot after acquiring all table locks.
	var original bool
	if err = tx.QueryRowContext(active, `SELECT current_database()=$1 AND NOT pg_is_in_recovery()
      AND EXISTS(SELECT 1 FROM pg_roles WHERE rolname=current_user AND (rolsuper OR rolbypassrls))`, database).Scan(&original); err != nil || !original {
		return ErrUpgradeFence
	}
	acquire, stopAcquire := context.WithTimeout(active, 10*time.Second)
	defer stopAcquire()
	var held bool
	if err = tx.QueryRowContext(acquire, `SELECT pg_try_advisory_xact_lock(73421019)`).Scan(&held); err != nil || !held {
		return ErrUpgradeFence
	}
	// Identifiers are exclusively the closed, compiled source table inventory.
	for _, table := range tables {
		if _, err = tx.ExecContext(acquire, `LOCK TABLE public.`+table+` IN SHARE MODE`); err != nil {
			return ErrUpgradeFence
		}
	}
	ids, err := upgradeFenceInventory(acquire, tx, tables)
	if err != nil {
		return ErrUpgradeFence
	}
	deadline, _ := active.Deadline()
	fence := &UpgradeFence{checkPermit: make(chan struct{}, 1), tx: tx, tables: tables, relationIDs: ids, database: database, source: slices.Clone(source), deadline: deadline, reviewerHistory: reviewerHistory, feedbackHistory: feedback, repairHistory: repair, configurationHistory: configuration}
	if fence.Check(acquire) != nil {
		return ErrUpgradeFence
	}
	if err = fence.inspectHistory(acquire); err != nil {
		return err
	}
	// Continuous observation cancels the original callback if the backend dies.
	// Check must still bracket every Kubernetes effect; a heartbeat is not a lease.
	done := make(chan struct{})
	stopHeartbeat := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-active.Done():
				return
			case <-stopHeartbeat:
				return
			case <-ticker.C:
				if fence.Check(active) != nil {
					cancel()
					return
				}
			}
		}
	}()
	stopped := false
	joinHeartbeat := func() {
		if !stopped {
			close(stopHeartbeat)
			<-done
			stopped = true
		}
	}
	defer func() { cancel(); joinHeartbeat() }()
	err = pause(active, fence)
	// No heartbeat may race the final inspection or acknowledged rollback.
	joinHeartbeat()
	if err != nil {
		return err
	}
	if active.Err() != nil || fence.Check(active) != nil {
		return ErrUpgradeFence
	}
	// No row writes were performed. Rollback releases only administrative locks.
	if err = tx.Rollback(); err != nil {
		return ErrUpgradeFence
	}
	return nil
}

// ExpiresAt is the original deadline, never renewed by protocol checks.
func (f *UpgradeFence) ExpiresAt() time.Time { return f.deadline }
