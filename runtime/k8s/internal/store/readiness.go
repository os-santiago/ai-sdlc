package store

import (
	"context"
	"errors"
	"io/fs"
)

// CheckMigrations observes schema readiness without DDL or privileged fallback.
// New components wait for the dedicated migrator to install their schema.
func (db *DB) CheckMigrations(ctx context.Context) error {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return errors.New("schema manifest unavailable")
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		var applied bool
		if err := db.Pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename=$1)`, entry.Name()).Scan(&applied); err != nil || !applied {
			return errors.New("required schema migration is not ready")
		}
	}
	return nil
}
