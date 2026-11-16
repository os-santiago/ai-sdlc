package store

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"
)

// This isolates the SQL scheduling window, not historical authority. The copied
// table retains types/checks/indexes but deliberately omits foreign keys. No
// provenance claim, network verification or operational record uses these IDs.
func TestProvenanceDispatchWindowSerializesAndExpiresWithoutClockRewrite(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	schema := "window_" + strings.ReplaceAll(NewID(), "-", "")
	if _, e := db.Pool.ExecContext(ctx, `CREATE SCHEMA `+schema); e != nil {
		t.Fatal(e)
	}
	defer db.Pool.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
	if _, e := db.Pool.ExecContext(ctx, `CREATE TABLE `+schema+`.provenance_handoffs (LIKE public.provenance_handoffs INCLUDING ALL)`); e != nil {
		t.Fatal(e)
	}
	reserve := func(id string) (bool, error) {
		inserted := false
		e := db.WithTx(ctx, func(tx *sql.Tx) error {
			if _, e := tx.ExecContext(ctx, `SET LOCAL search_path TO `+schema); e != nil {
				return e
			}
			busy, e := provenanceWindowBusy(ctx, tx)
			if e != nil || busy {
				return e
			}
			_, e = tx.ExecContext(ctx, `INSERT INTO provenance_handoffs(build_id,command_id,payload_digest) VALUES($1,$2,$3)`, "build-"+id, "verify-"+id, strings.Repeat("a", 64))
			inserted = e == nil
			return e
		})
		return inserted, e
	}
	start := make(chan struct{})
	type result struct {
		id       string
		inserted bool
		e        error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, id := range []string{strings.Repeat("1", 64), strings.Repeat("2", 64)} {
		wg.Add(1)
		go func(id string) { defer wg.Done(); <-start; ok, e := reserve(id); results <- result{id, ok, e} }(id)
	}
	close(start)
	wg.Wait()
	close(results)
	winner, loser := "", ""
	for r := range results {
		if r.e != nil {
			t.Fatal(r.e)
		}
		if r.inserted {
			if winner != "" {
				t.Fatal("concurrent fresh windows")
			}
			winner = r.id
		} else {
			loser = r.id
		}
	}
	if winner == "" || loser == "" {
		t.Fatal("reservation did not select exactly one")
	}
	if ok, e := reserve(loser); e != nil || ok {
		t.Fatal("busy window released early", e)
	}
	timer := time.NewTimer(126 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if ok, e := reserve(loser); e != nil || !ok {
		t.Fatal("real expiration did not release next distinct build", e)
	}
	var count int
	if e := db.Pool.QueryRowContext(ctx, `SELECT count(*) FROM `+schema+`.provenance_handoffs`).Scan(&count); e != nil || count != 2 {
		t.Fatal("original history replaced", count, e)
	}
	// The original immutable primary key still prevents a renewed handoff, even
	// after its scheduling window. No timestamp is changed to obtain this result.
	if _, e := db.Pool.ExecContext(ctx, `INSERT INTO `+schema+`.provenance_handoffs SELECT * FROM `+schema+`.provenance_handoffs WHERE build_id=$1`, "build-"+winner); e == nil {
		t.Fatal("original build reopened")
	}
}
