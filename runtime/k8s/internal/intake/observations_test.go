package intake

import (
	"context"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
)

func TestIssueObservationRefreshesOnlyWhenTargetWasRead(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if n, err := f.poller.PollOnce(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("first observation: %d %v", n, err)
	}
	if _, err := f.admin.Pool.Exec(`UPDATE intake_observations SET observed_at=clock_timestamp()-interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	var before time.Time
	if err := f.admin.Pool.QueryRow(`SELECT observed_at FROM intake_observations`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	f.source.pageHook = func(time.Time, int) []forge.Issue { return nil }
	f.due(t)
	if n, err := f.poller.PollOnce(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("omitted target scan: %d %v", n, err)
	}
	var observed, watermark time.Time
	if err := f.admin.Pool.QueryRow(`SELECT o.observed_at,c.watermark FROM intake_observations o JOIN intake_cursors c USING(repository,config_digest)`).Scan(&observed, &watermark); err != nil {
		t.Fatal(err)
	}
	if !observed.Equal(before) || !watermark.After(before.Add(9*time.Minute)) {
		t.Fatal("global scan manufactured an individual observation")
	}
	f.source.pageHook = nil
	f.due(t)
	if n, err := f.poller.PollOnce(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("unchanged target observation: %d %v", n, err)
	}
	var generation int64
	if err := f.admin.Pool.QueryRow(`SELECT observed_at,generation FROM intake_observations`).Scan(&observed, &generation); err != nil {
		t.Fatal(err)
	}
	if !observed.After(before.Add(9*time.Minute)) || generation != 1 {
		t.Fatal("dedupe did not refresh actual target observation")
	}
	if records, events := f.counts(t); records != 1 || events != 1 {
		t.Fatal("observation refresh duplicated business intake")
	}
}

func TestIssueObservationKeepsActualReadTimeAndRejectsExpiredLease(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.poller.PollOnce(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	lease, err := f.poller.reserve(ctx, time.Now())
	if err != nil || lease == nil {
		t.Fatal("fixture lease unavailable")
	}
	snapshot, err := f.poller.snapshot(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	readAt := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Microsecond)
	if _, err = f.poller.record(ctx, snapshot, lease.token, readAt); err != nil {
		t.Fatal(err)
	}
	var observed time.Time
	if err = f.admin.Pool.QueryRow(`SELECT observed_at FROM intake_observations`).Scan(&observed); err != nil {
		t.Fatal(err)
	}
	if !observed.Equal(readAt) {
		t.Fatal("persistence time replaced authoritative read time")
	}
	if _, err = f.admin.Pool.Exec(`UPDATE intake_cursors SET lease_until=clock_timestamp()+interval '1500 milliseconds'`); err != nil {
		t.Fatal(err)
	}
	lock, err := f.admin.Pool.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err = lock.Exec(`SELECT repository FROM intake_heads FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.poller.record(ctx, snapshot, lease.token, time.Now()); done <- err }()
	deadline := time.Now().Add(time.Second)
	for {
		var waiting int
		if err = f.admin.Pool.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE usename='aisdlc_intake' AND wait_event_type='Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("observation never reached actual head lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(1600 * time.Millisecond)
	if err = lock.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err == nil {
		t.Fatal("expired lease refreshed observation after lock wait")
	}
	if err = f.admin.Pool.QueryRow(`SELECT observed_at FROM intake_observations`).Scan(&observed); err != nil {
		t.Fatal(err)
	}
	if !observed.Equal(readAt) {
		t.Fatal("expired lease changed observed timestamp")
	}
}
