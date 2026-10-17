// Package store is the durable-state boundary for the k8s/OpenShift control
// plane: one PostgreSQL database, one schema per component's aggregate, and
// the transactional outbox (internal/outbox) every component writes to in
// the same transaction as its own state.
//
// See docs/architecture/adr/0005-runtime-k8s-event-bus-and-engine-invocation.md
// for why PostgreSQL + outbox (not a message broker) and why CloudNativePG.
//
// This is the one package in runtime/k8s allowed to import a third-party
// driver (jackc/pgx/v5/stdlib). The pure domain packages (admission, cycle,
// planning, gates) never import store — they stay dependency-free and
// synchronously testable exactly as ADR-0004 left them.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Querier is the subset of *sql.DB and *sql.Tx every repository function in
// this package needs. Passing a *sql.Tx lets a component persist its state
// and append an outbox record (internal/outbox.Append) atomically; passing
// the *DB.Pool directly is fine for read-only queries.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DB wraps a connection pool opened against DATABASE_URL.
type DB struct {
	Pool *sql.DB
}

// Open connects to Postgres and verifies the connection with a bounded ping.
// Callers should treat a non-nil error as "run without persistence" at
// their own discretion (every component in this runtime degrades to
// synchronous-only behavior when its *DB is nil — see each component's
// Deps type) rather than crash the process, matching
// internal/contract.LoadFromEnv's "degrade, never crash a running pod"
// posture.
func Open(ctx context.Context, databaseURL string) (*DB, error) {
	pool, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	pool.SetConnMaxLifetime(30 * time.Minute)
	pool.SetMaxOpenConns(10)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.PingContext(pingCtx); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &DB{Pool: pool}, nil
}

// Close releases the connection pool.
func (db *DB) Close() error {
	if db == nil || db.Pool == nil {
		return nil
	}
	return db.Pool.Close()
}

// Migrate applies every embedded migrations/*.sql file, in filename order,
// inside a single transaction tracked by a schema_migrations table. It is
// safe to call on every component startup: already-applied files are
// skipped, and CREATE TABLE IF NOT EXISTS statements make each file
// idempotent even without the tracking table.
func (db *DB) Migrate(ctx context.Context) error {
	if _, err := db.Pool.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		filename   text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var applied bool
		row := db.Pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename = $1)`, name)
		if err := row.Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if applied {
			continue
		}

		sqlBytes, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		tx, err := db.Pool.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (filename) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

// WithTx runs fn inside a transaction, committing on success and rolling
// back on error or panic. Every "persist state + append outbox record"
// write path in this runtime goes through this helper so the two writes
// are always atomic.
func (db *DB) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.Pool.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// NewID returns a random RFC 4122 version-4 UUID string. Generated locally
// (crypto/rand, no third-party uuid library) so this package's only new
// dependency stays the Postgres driver.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
