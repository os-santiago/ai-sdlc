package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// A configuration-only transition (no migration) admits completed history of
// every stage, including feedback, repair and browser records, and refuses
// only in-flight model-dependent work.
func TestConfigurationUpgradeFenceAdmitsCompletedHistoryAndRejectsInFlightWork(t *testing.T) {
	db, name, source := upgradeFenceFixture(t, 37)
	ctx := context.Background()
	if _, err := db.Pool.Exec(browserBridgeHistory); err != nil {
		t.Fatal(err)
	}
	before := upgradeHistory(t, db)
	hold := func() error {
		return db.WithConfigurationUpgradeFence(ctx, name, source, func(ctx context.Context, f *UpgradeFence) error {
			writer, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			defer cancel()
			if _, err := db.Pool.ExecContext(writer, `UPDATE public.work_items SET status=status`); err == nil {
				t.Fatal("writer escaped")
			}
			return f.Check(ctx)
		})
	}
	if err := hold(); err != nil {
		t.Fatal("completed history refused", err)
	}
	if upgradeHistory(t, db) != before {
		t.Fatal("historical rows changed")
	}
	tables, err := repairScopedUpgradeFenceTables(source, false, false, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range append(append(slices.Clone(browserUpgradeTables), "review_feedback"), repairUpgradeTables...) {
		tx, err := db.Pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(`ALTER TABLE public.` + table + ` RENAME TO configuration_original; CREATE TABLE public.` + table + `(fixture boolean); INSERT INTO public.` + table + ` VALUES(true)`); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		f := &UpgradeFence{tx: tx, source: source, tables: tables, configurationHistory: true}
		if err = f.inspectHistory(ctx); err != nil {
			tx.Rollback()
			t.Fatal("completed history refused", table, err)
		}
		if err = tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	for _, live := range []string{
		`UPDATE review_runs SET cleanup_state='pending'`,
		`UPDATE review_model_calls SET state='started', response_digest=NULL, output_bytes=NULL, observation=NULL, handed_off_at=NULL`,
	} {
		tx, err := db.Pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(live); err != nil {
			tx.Rollback()
			t.Fatal(live, err)
		}
		f := &UpgradeFence{tx: tx, source: source, tables: tables, configurationHistory: true}
		if err = f.inspectHistory(ctx); !errors.Is(err, ErrUpgradeFenceHistory) {
			tx.Rollback()
			t.Fatal("in-flight work admitted", live, err)
		}
		if err = tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Pool.Exec(`UPDATE review_runs SET cleanup_state='pending'`); err != nil {
		t.Fatal(err)
	}
	called := false
	if err = db.WithConfigurationUpgradeFence(ctx, name, source, func(context.Context, *UpgradeFence) error { called = true; return nil }); !errors.Is(err, ErrUpgradeFenceHistory) || called {
		t.Fatal("uncleaned review admitted", err)
	}
	if err = db.WithConfigurationUpgradeFence(ctx, name, source[:36], func(context.Context, *UpgradeFence) error { return nil }); err == nil || errors.Is(err, ErrUpgradeFenceHistory) {
		t.Fatal("configuration fence crossed a migration", err)
	}
}
