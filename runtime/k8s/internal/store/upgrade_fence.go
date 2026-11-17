package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var ErrUpgradeFence = errors.New("runtime upgrade fence denied")

// UpgradeFence exists only during its original bounded transaction. It grants
// no delivery, migration or rollback authority. Call Check immediately before
// and after each original-identity-bound administrative Kubernetes operation.
type UpgradeFence struct {
	tx          *sql.Tx
	tables      []string
	relationIDs string
	database    string
	source      []MigrationIdentity
	deadline    time.Time
}

// This reviewed bridge covers schemas 0033 and 0034, before any candidate or
// downstream external delivery history. Additional schemas/states need their
// own compatibility and recovery proof; no history is deleted to qualify.
func upgradeFenceTables(source []MigrationIdentity) ([]string, error) {
	catalog, err := MigrationCatalog()
	if err != nil || len(catalog) != 34 || catalog[32].Filename != "0033_production_completion.sql" || catalog[33].Filename != "0034_execution_failures.sql" || ValidateMigrationSource(source) != nil || (len(source) != 33 && len(source) != 34) {
		return nil, ErrUpgradeFence
	}
	tables := slices.Clone(bootstrapTables)
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
	if f == nil || f.tx == nil || time.Now().After(f.deadline) {
		return ErrUpgradeFence
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
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
		if err = f.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.`+name+`)`).Scan(&exists); err != nil || exists {
			return ErrUpgradeFence
		}
	}
	var blocked bool
	if err = f.tx.QueryRowContext(ctx, `SELECT
  EXISTS(SELECT 1 FROM public.execution_requests WHERE state='claimed')
  OR EXISTS(SELECT 1 FROM public.execution_lifecycles l JOIN public.execution_requests r ON r.id=l.request_id WHERE r.state<>'stopped' OR l.cleanup_state<>'complete')
  OR EXISTS(SELECT 1 FROM public.execution_effects WHERE result IS NULL)
  OR EXISTS(SELECT 1 FROM public.model_budgets WHERE state<>'stopped')
  OR EXISTS(SELECT 1 FROM public.model_calls WHERE state NOT IN('completed','stopped'))`).Scan(&blocked); err != nil || blocked {
		return ErrUpgradeFence
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
	tables, err := upgradeFenceTables(source)
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
	fence := &UpgradeFence{tx: tx, tables: tables, relationIDs: ids, database: database, source: slices.Clone(source), deadline: deadline}
	if fence.Check(acquire) != nil || fence.inspectHistory(acquire) != nil {
		return ErrUpgradeFence
	}
	// Continuous observation cancels the original callback if the backend dies.
	// Check must still bracket every Kubernetes effect; a heartbeat is not a lease.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-active.Done():
				return
			case <-ticker.C:
				if fence.Check(active) != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-done }()
	if err = pause(active, fence); err != nil {
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
