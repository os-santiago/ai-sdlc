package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestUpgradeFenceFeedbackBridgeClosedSchemas(t *testing.T) {
	catalog, err := MigrationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{33, 34, 35, 36} {
		_, legacy := upgradeFenceTables(catalog[:n])
		_, browser := scopedUpgradeFenceTables(catalog[:n], false, true)
		if (legacy == nil) != (n == 33 || n == 34) || (browser == nil) != (n == 35 || n == 36) {
			t.Fatal("schema authority crossed", n)
		}
	}
	bad := slices.Clone(catalog[:35])
	bad[0].SHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := scopedUpgradeFenceTables(bad, false, true); err == nil {
		t.Fatal("changed prefix accepted")
	}
}
func TestUpgradeFenceFeedbackBridgePreservesContainedHistory(t *testing.T) {
	for _, count := range []int{35, 36} {
		db, name, source := upgradeFenceFixture(t, count)
		ctx := context.Background()
		if _, err := db.Pool.ExecContext(ctx, browserBridgeHistory); err != nil {
			t.Fatal(err)
		}
		before := upgradeHistory(t, db)
		check := func() {
			t.Helper()
			if err := db.WithReviewFeedbackUpgradeFence(ctx, name, source, func(ctx context.Context, f *UpgradeFence) error {
				writer, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
				if _, err := db.Pool.ExecContext(writer, `UPDATE public.work_items SET status=status`); err == nil {
					t.Fatal("writer escaped")
				}
				if err := db.WithReviewFeedbackUpgradeFence(ctx, name, source, func(context.Context, *UpgradeFence) error { return nil }); err == nil {
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
		if err := db.WithReviewFeedbackUpgradeFence(ctx, name, source, func(context.Context, *UpgradeFence) error { called = true; return nil }); !errors.Is(err, ErrUpgradeFenceHistory) || called {
			t.Fatal("unknown effect accepted", err)
		}
		if _, err := db.Pool.Exec(`DELETE FROM review_effects`); err != nil {
			t.Fatal(err)
		}
		check()
		if count == 36 {
			if err := db.WithContainedReviewUpgradeFence(ctx, name, source, func(context.Context, *UpgradeFence) error { called = true; return nil }); err == nil {
				t.Fatal("legacy reviewer accepted36")
			}
			if err := db.WithPreCandidateUpgradeFence(ctx, name, source[:34], func(context.Context, *UpgradeFence) error { called = true; return nil }); err == nil {
				t.Fatal("schema34 adopted feedback relations")
			}
			for _, table := range append(slices.Clone(browserUpgradeTables), "review_feedback") {
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
				tables, err := scopedUpgradeFenceTables(source, false, true)
				if err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				f := &UpgradeFence{tx: tx, source: source, tables: tables, reviewerHistory: true}
				if err = f.inspectHistory(ctx); !errors.Is(err, ErrUpgradeFenceHistory) {
					tx.Rollback()
					t.Fatal("feedback/browser row accepted", table, err)
				}
				if err = tx.Rollback(); err != nil {
					t.Fatal(err)
				}
				check()
			}
		}
	}
}

func TestUpgradeFenceFeedbackRetainsBlockedCandidateOnly(t *testing.T) {
	for _, count := range []int{35, 36} {
		db, name, source := upgradeFenceFixture(t, count)
		ctx := context.Background()
		if _, err := db.Pool.Exec(browserBridgeHistory + `UPDATE work_items SET status='blocked' WHERE id='contained-review';`); err != nil {
			t.Fatal(err)
		}
		before := upgradeHistory(t, db)
		if err := db.WithReviewFeedbackUpgradeFence(ctx, name, source, func(ctx context.Context, f *UpgradeFence) error { return f.Check(ctx) }); err != nil || upgradeHistory(t, db) != before {
			t.Fatal("contained blocked candidate changed or rejected", err)
		}
		var legacy bool
		if err := db.Pool.QueryRow(reviewerUpgradeHistory).Scan(&legacy); err != nil || legacy {
			t.Fatal("historical reviewer predicate widened", err)
		}
		if count == 35 {
			if err := db.WithBrowserDeploymentUpgradeFence(ctx, name, source, func(context.Context, *UpgradeFence) error {
				t.Fatal("legacy browser adopted blocked history")
				return nil
			}); !errors.Is(err, ErrUpgradeFenceHistory) {
				t.Fatal("legacy browser boundary changed", err)
			}
		}
		for _, change := range []string{
			`UPDATE work_items SET status='blocked' WHERE id='ready'`,
			`UPDATE review_runs SET cleanup_state='uncertain'`,
			`UPDATE review_runs SET expires_at=clock_timestamp()+interval '1 hour'`,
			`INSERT INTO review_effects(run_id,kind,token,resource_name) SELECT id,'export',gen_random_uuid(),'unacknowledged' FROM review_runs`,
		} {
			// Owned rollback-only fixture observations exercise the exact predicate;
			// SQL or JSON errors are never counted as a successful denial.
			tx, err := db.Pool.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(change); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			var safe bool
			if err = tx.QueryRow(reviewFeedbackUpgradeHistory).Scan(&safe); err != nil || safe {
				tx.Rollback()
				t.Fatal("unsafe blocked history accepted", change, err)
			}
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
		}
		if upgradeHistory(t, db) != before {
			t.Fatal("negative controls changed retained history")
		}
	}
}
