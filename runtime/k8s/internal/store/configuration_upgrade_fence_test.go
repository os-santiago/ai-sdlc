package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// A configuration-only transition (no migration) admits completed history of
// every stage, including feedback, repair and browser records, and refuses
// only in-flight model-dependent work.
func TestConfigurationUpgradeFenceAdmitsCompletedHistoryAndRejectsInFlightWork(t *testing.T) {
	// The current catalog, and the current catalog without its relaxation
	// migrations (which the configuration upgrade's migrator job applies).
	for _, count := range []int{41, 40, 39, 38, 37} {
		t.Run(fmt.Sprint(count), func(t *testing.T) { configurationFenceAt(t, count) })
	}
}

func TestConfigurationSourceAdmitsOnlyMissingRelaxations(t *testing.T) {
	catalog, err := MigrationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for n := 30; n <= len(catalog); n++ {
		if configurationSource(catalog[:n]) != (n >= len(catalog)-len(RelaxationMigrations)) {
			t.Fatal("configuration source", n)
		}
	}
	changed := slices.Clone(catalog)
	changed[len(changed)-1].SHA256 = strings.Repeat("0", 64)
	if configurationSource(changed) {
		t.Fatal("a changed migration was admitted")
	}
}

func configurationFenceAt(t *testing.T, count int) {
	db, name, source := upgradeFenceFixture(t, count)
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
	history := append(append(slices.Clone(browserUpgradeTables), "review_feedback"), slices.DeleteFunc(slices.Clone(repairUpgradeTables), func(s string) bool { return s == "repair_model_budgets" })...)
	for _, table := range history {
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
	for state, admitted := range map[string]bool{"stopped": true, "reserved": false, "active": false} {
		tx, err := db.Pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(`ALTER TABLE public.repair_model_budgets RENAME TO configuration_original; CREATE TABLE public.repair_model_budgets(state text); INSERT INTO public.repair_model_budgets VALUES('` + state + `')`); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		f := &UpgradeFence{tx: tx, source: source, tables: tables, configurationHistory: true}
		err = f.inspectHistory(ctx)
		tx.Rollback()
		if (err == nil) != admitted || (!admitted && !errors.Is(err, ErrUpgradeFenceHistory)) {
			t.Fatal("repair budget", state, err)
		}
	}
	// A terminal ambiguous model call (never replayed) is history; a started
	// call is in flight.
	for state, admitted := range map[string]bool{"ambiguous": true, "stopped": true, "started": false, "reserved": false} {
		tx, err := db.Pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(`INSERT INTO model_calls(allocation_id,sequence,request_sha256,input_bytes,response_limit,expires_at,state)
			SELECT id,1,repeat('a',64),1,1,clock_timestamp()-interval '1 hour',$1 FROM model_allocations LIMIT 1`, state); err != nil {
			tx.Rollback()
			t.Fatal(state, err)
		}
		f := &UpgradeFence{tx: tx, source: source, tables: tables, configurationHistory: true}
		err = f.inspectHistory(ctx)
		tx.Rollback()
		if (err == nil) != admitted || (!admitted && !errors.Is(err, ErrUpgradeFenceHistory)) {
			t.Fatal("model call", state, err)
		}
	}
	// A call left started by a review that stopped, was cleaned up and whose
	// window expired can never be handed off: it is history, not in flight.
	{
		tx, err := db.Pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(`UPDATE review_model_calls SET state='started', response_digest=NULL, output_bytes=NULL, observation=NULL, handed_off_at=NULL`); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		f := &UpgradeFence{tx: tx, source: source, tables: tables, configurationHistory: true}
		err = f.inspectHistory(ctx)
		tx.Rollback()
		if err != nil {
			t.Fatal("terminal review call refused", err)
		}
	}
	const startedCall = `UPDATE review_model_calls SET state='started', response_digest=NULL, output_bytes=NULL, observation=NULL, handed_off_at=NULL;`
	for _, live := range []string{
		`UPDATE review_runs SET cleanup_state='pending'`,
		startedCall + `UPDATE review_runs SET expires_at=clock_timestamp()+interval '1 hour'`,
		// Also refused by the review_runs clause; kept as defense in depth.
		startedCall + `UPDATE review_runs SET cleanup_state='pending'`,
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
