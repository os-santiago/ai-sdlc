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
}

// This reviewed bridge covers schemas 0033 and 0034, before any candidate or
// downstream external delivery history. Additional schemas/states need their
// own compatibility and recovery proof; no history is deleted to qualify.
var browserUpgradeTables = []string{"deployment_browser_cleanups", "deployment_browser_evidence", "deployment_browser_intents", "deployment_browser_pods", "deployment_browser_sends"}
var repairUpgradeTables = []string{"execution_repairs", "repair_model_budgets", "review_repair_handoffs", "review_repair_stops"}

func upgradeFenceTables(source []MigrationIdentity) ([]string, error) {
	return selectedUpgradeFenceTables(source, false)
}

func selectedUpgradeFenceTables(source []MigrationIdentity, browser bool) ([]string, error) {
	catalog, err := MigrationCatalog()
	if err != nil || len(catalog) != 37 || catalog[36].Filename != "0037_review_repair.sql" || catalog[35].Filename != "0036_review_feedback.sql" || catalog[34].Filename != "0035_deployment_browser.sql" || catalog[32].Filename != "0033_production_completion.sql" || catalog[33].Filename != "0034_execution_failures.sql" || ValidateMigrationSource(source) != nil || (!browser && len(source) != 33 && len(source) != 34) || (browser && len(source) != 34 && len(source) != 35) {
		return nil, ErrUpgradeFence
	}
	// Historical bridges retain their exact source schemas; neither feedback
	// nor repair history may be admitted by a newer embedded catalog.
	tables := slices.DeleteFunc(slices.Clone(bootstrapTables), func(s string) bool { return s == "review_feedback" || slices.Contains(repairUpgradeTables, s) })
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
	allowed := map[string]bool{}
	if f.reviewerHistory {
		var safe bool
		if (len(f.source) != 34 && len(f.source) != 35) || f.tx.QueryRowContext(ctx, reviewerUpgradeHistory).Scan(&safe) != nil {
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
	tables, err := selectedUpgradeFenceTables(source, browser)
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
	fence := &UpgradeFence{checkPermit: make(chan struct{}, 1), tx: tx, tables: tables, relationIDs: ids, database: database, source: slices.Clone(source), deadline: deadline, reviewerHistory: reviewerHistory}
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
