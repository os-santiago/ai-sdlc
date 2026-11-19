package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestUpgradeFenceRepairClosedSchemas(t *testing.T) {
	catalog, err := MigrationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for n := 33; n <= 37; n++ {
		_, err = repairScopedUpgradeFenceTables(catalog[:n], false, false, true)
		if (err == nil) != (n == 36 || n == 37) {
			t.Fatal("repair schema crossed", n)
		}
		if n == 37 {
			if _, err = scopedUpgradeFenceTables(catalog[:n], false, true); err == nil {
				t.Fatal("legacy feedback widened")
			}
		}
	}
	bad := slices.Clone(catalog[:36])
	bad[0].SHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err = repairScopedUpgradeFenceTables(bad, false, false, true); err == nil {
		t.Fatal("changed prefix")
	}
}

func TestUpgradeFenceRepairPreservesHistoryAndRejectsPriorFeedback(t *testing.T) {
	for _, count := range []int{36, 37} {
		db, name, source := upgradeFenceFixture(t, count)
		ctx := context.Background()
		if _, err := db.Pool.Exec(browserBridgeHistory); err != nil {
			t.Fatal(err)
		}
		before := upgradeHistory(t, db)
		check := func() {
			t.Helper()
			if err := db.WithReviewRepairUpgradeFence(ctx, name, source, func(ctx context.Context, f *UpgradeFence) error {
				writer, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
				if _, err := db.Pool.ExecContext(writer, `UPDATE public.work_items SET status=status`); err == nil {
					t.Fatal("writer escaped")
				}
				return f.Check(ctx)
			}); err != nil {
				t.Fatal("closed repair bridge", count, err)
			}
			if upgradeHistory(t, db) != before {
				t.Fatal("historical rows changed")
			}
		}
		check()
		tables, err := repairScopedUpgradeFenceTables(source, false, false, true)
		if err != nil {
			t.Fatal(err)
		}
		protected := append(slices.Clone(browserUpgradeTables), "review_feedback")
		if count == 37 {
			protected = append(protected, repairUpgradeTables...)
		}
		for _, table := range protected {
			tx, err := db.Pool.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(`ALTER TABLE public.` + table + ` RENAME TO bridge_original; CREATE TABLE public.` + table + `(fixture boolean); INSERT INTO public.` + table + ` VALUES(true)`); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			f := &UpgradeFence{tx: tx, source: source, tables: tables, reviewerHistory: true, repairHistory: true}
			if err = f.inspectHistory(ctx); !errors.Is(err, ErrUpgradeFenceHistory) {
				tx.Rollback()
				t.Fatal("historical feedback/repair/browser adopted", table, err)
			}
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
		}
		check()
		if _, err = db.Pool.Exec(`UPDATE review_runs SET expires_at=clock_timestamp()+interval '1 hour'`); err != nil {
			t.Fatal(err)
		}
		called := false
		if err = db.WithReviewRepairUpgradeFence(ctx, name, source, func(context.Context, *UpgradeFence) error { called = true; return nil }); !errors.Is(err, ErrUpgradeFenceHistory) || called {
			t.Fatal("live review admitted", err)
		}
	}
}
