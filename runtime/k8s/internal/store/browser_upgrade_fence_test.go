package store

import (
	"context"
	_ "embed"
	"errors"
	"slices"
	"testing"
	"time"
)

//go:embed testdata/browser_upgrade_history.sql
var browserBridgeHistory string

func TestUpgradeFenceBrowserBridgeClosedSchemas(t *testing.T) {
	catalog, err := MigrationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{33, 34, 35, 36} {
		_, legacy := upgradeFenceTables(catalog[:n])
		_, browser := selectedUpgradeFenceTables(catalog[:n], true)
		if (legacy == nil) != (n == 33 || n == 34) || (browser == nil) != (n == 34 || n == 35) {
			t.Fatal("schema authority crossed", n)
		}
	}
	bad := slices.Clone(catalog[:34])
	bad[0].SHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := selectedUpgradeFenceTables(bad, true); err == nil {
		t.Fatal("changed prefix accepted")
	}
}
func TestUpgradeFenceBrowserBridgePreservesContainedHistory(t *testing.T) {
	for _, count := range []int{34, 35} {
		db, name, source := upgradeFenceFixture(t, count)
		ctx := context.Background()
		if _, err := db.Pool.ExecContext(ctx, browserBridgeHistory); err != nil {
			t.Fatal(err)
		}
		before := upgradeHistory(t, db)
		check := func() {
			t.Helper()
			if err := db.WithBrowserDeploymentUpgradeFence(ctx, name, source, func(ctx context.Context, f *UpgradeFence) error {
				writer, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
				if _, err := db.Pool.ExecContext(writer, `UPDATE public.work_items SET status=status`); err == nil {
					t.Fatal("writer escaped")
				}
				if err := db.WithBrowserDeploymentUpgradeFence(ctx, name, source, func(context.Context, *UpgradeFence) error { return nil }); err == nil {
					t.Fatal("replica escaped")
				}
				return f.Check(ctx)
			}); err != nil {
				t.Fatal("bridge rejected contained history", count, err)
			}
			if upgradeHistory(t, db) != before {
				t.Fatal("history changed")
			}
		}
		check()
		if _, err := db.Pool.Exec(`INSERT INTO review_effects(run_id,kind,token,resource_name) SELECT id,'export',$1,'review-fixture' FROM review_runs`, NewID()); err != nil {
			t.Fatal(err)
		}
		called := false
		if err := db.WithBrowserDeploymentUpgradeFence(ctx, name, source, func(context.Context, *UpgradeFence) error { called = true; return nil }); !errors.Is(err, ErrUpgradeFenceHistory) || called {
			t.Fatal("unknown effect accepted", err)
		}
		if _, err := db.Pool.Exec(`DELETE FROM review_effects`); err != nil {
			t.Fatal(err)
		}
		check()
		if count == 35 {
			if err := db.WithContainedReviewUpgradeFence(ctx, name, source, func(context.Context, *UpgradeFence) error { called = true; return nil }); err == nil {
				t.Fatal("legacy reviewer accepted35")
			}
			if err := db.WithPreCandidateUpgradeFence(ctx, name, source[:34], func(context.Context, *UpgradeFence) error { called = true; return nil }); err == nil {
				t.Fatal("schema34 adopted browser relations")
			}
			for _, table := range browserUpgradeTables {
				// A disposable table replacement permits a minimal row independently of
				// FK ordering; the closed fence must reject it before the callback.
				tx, err := db.Pool.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = tx.Exec(`ALTER TABLE public.` + table + ` RENAME TO bridge_original; CREATE TABLE public.` + table + `(fixture boolean); INSERT INTO public.` + table + ` VALUES(true)`); err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				// Query the history predicate in the same transaction to exercise EACH
				// table's emptiness independently without weakening any production FK.
				f := &UpgradeFence{tx: tx, source: source, tables: bootstrapTables, reviewerHistory: true}
				if err = f.inspectHistory(ctx); !errors.Is(err, ErrUpgradeFenceHistory) {
					tx.Rollback()
					t.Fatal("browser row accepted", table, err)
				}
				if err = tx.Rollback(); err != nil {
					t.Fatal(err)
				}
				check()
			}
		}
	}
}
