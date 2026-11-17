package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"time"
)

var ErrUpgradeInspection = errors.New("upgrade migration observation denied")
var migrationName = regexp.MustCompile(`^[0-9]{4}_[a-z0-9_]+\.sql$`)

// MigrationIdentity describes expected source bytes, not historical execution.
type MigrationIdentity struct {
	Filename string `json:"filename"`
	SHA256   string `json:"sha256"`
}

func MigrationCatalog() ([]MigrationIdentity, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil || len(entries) == 0 || len(entries) > 512 {
		return nil, ErrUpgradeInspection
	}
	result := make([]MigrationIdentity, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !migrationName.MatchString(entry.Name()) {
			return nil, ErrUpgradeInspection
		}
		raw, err := migrationsFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, ErrUpgradeInspection
		}
		result = append(result, MigrationIdentity{entry.Name(), fmt.Sprintf("%x", sha256.Sum256(raw))})
	}
	return result, nil
}

// ValidateMigrationSource requires the exact old prefix of this binary's
// embedded catalog. The caller must independently bind that source catalog to
// its reviewed original publication. A supplied catalog is not an attestation.
func ValidateMigrationSource(source []MigrationIdentity) error {
	target, err := MigrationCatalog()
	if err != nil || len(source) == 0 || len(source) > len(target) || !slices.Equal(source, target[:len(source)]) {
		return ErrUpgradeInspection
	}
	return nil
}

type UpgradeMigrationObservation struct {
	Version                     string              `json:"version"`
	State                       string              `json:"state"`
	ObservedAt                  time.Time           `json:"observedAt"`
	SourceMigrationCount        int                 `json:"sourceMigrationCount"`
	TargetMigrationCount        int                 `json:"targetMigrationCount"`
	RecordedMigrations          []string            `json:"recordedMigrations"`
	RemainingMigrations         []MigrationIdentity `json:"remainingMigrations"`
	HistoricalSQLHashesVerified bool                `json:"historicalSQLHashesVerified"`
	SchemaIntegrityVerified     bool                `json:"schemaIntegrityVerified"`
	ProducerExclusionVerified   bool                `json:"producerExclusionVerified"`
	UpgradeAuthorized           bool                `json:"upgradeAuthorized"`
	RollbackAuthorized          bool                `json:"rollbackAuthorized"`
	WorkflowEvidence            bool                `json:"workflowEvidence"`
}

// InspectUpgradeMigrations reads one primary snapshot, permitting existing
// workflow history. It never migrates, repairs tracking rows, drains producers,
// claims work or establishes schema compatibility. This observation is not a
// lock against concurrent migration or workflow changes after the snapshot.
func (db *DB) InspectUpgradeMigrations(ctx context.Context, database string, source []MigrationIdentity) (UpgradeMigrationObservation, error) {
	deny := func() (UpgradeMigrationObservation, error) {
		return UpgradeMigrationObservation{}, ErrUpgradeInspection
	}
	if db == nil || db.Pool == nil || database == "" || len(database) > 63 || ValidateMigrationSource(source) != nil {
		return deny()
	}
	target, err := MigrationCatalog()
	if err != nil {
		return deny()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := db.Pool.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable, ReadOnly: true})
	if err != nil {
		return deny()
	}
	defer tx.Rollback()
	for _, statement := range []string{`SET LOCAL statement_timeout='3000ms'`, `SET LOCAL lock_timeout='1000ms'`, `SET LOCAL search_path=pg_catalog`, `SET LOCAL row_security=off`} {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return deny()
		}
	}
	// Acquire relation custody before the first snapshot query. A tracker
	// replacement while waiting must not be validated against an older catalog.
	if _, err = tx.ExecContext(ctx, `LOCK TABLE public.schema_migrations IN ACCESS SHARE MODE`); err != nil {
		return deny()
	}
	var valid bool
	if err = tx.QueryRowContext(ctx, `SELECT current_database()=$1 AND NOT pg_is_in_recovery()
   AND EXISTS(SELECT 1 FROM pg_roles WHERE rolname=current_user AND (rolsuper OR rolbypassrls))`, database).Scan(&valid); err != nil || !valid {
		return deny()
	}
	// Do not execute a view or foreign relation substituted for the tracker.
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE c.oid=to_regclass('public.schema_migrations') AND n.nspname='public' AND c.relname='schema_migrations' AND c.relkind='r' AND c.relpersistence='p')`).Scan(&valid); err != nil || !valid {
		return deny()
	}
	rows, err := tx.QueryContext(ctx, `SELECT CASE WHEN octet_length(filename)<=128 THEN filename ELSE NULL END FROM public.schema_migrations ORDER BY filename COLLATE "C" LIMIT 513`)
	if err != nil {
		return deny()
	}
	names := make([]string, 0, len(target))
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			rows.Close()
			return deny()
		}
		names = append(names, name)
	}
	readErr := rows.Err()
	rows.Close()
	if readErr != nil || len(names) < len(source) || len(names) > len(target) {
		return deny()
	}
	for i, name := range names {
		if name != target[i].Filename {
			return deny()
		}
	}
	state := "partial-migration-recorded"
	if len(names) == len(source) {
		state = "source-recorded"
	} else if len(names) == len(target) {
		state = "target-recorded"
	}
	report := UpgradeMigrationObservation{Version: "ai-sdlc.upgrade-migration-observation/v1", State: state,
		SourceMigrationCount: len(source), TargetMigrationCount: len(target), RecordedMigrations: names,
		RemainingMigrations: append([]MigrationIdentity{}, target[len(names):]...)}
	if err = tx.QueryRowContext(ctx, `SELECT transaction_timestamp()`).Scan(&report.ObservedAt); err != nil {
		return deny()
	}
	if tx.Commit() != nil {
		return deny()
	}
	return report, nil
}
