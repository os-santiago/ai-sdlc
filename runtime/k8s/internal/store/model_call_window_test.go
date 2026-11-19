package store

import (
	"context"
	"testing"
	"time"
)

// The reservation expiry follows the profile's per-call window, bounded by the
// allocation; invalid windows are refused before any ledger write.
func TestModelCallReservationFollowsTheCallWindow(t *testing.T) {
	db, b, a := modelFixture(t, 4)
	ctx := context.Background()
	for _, window := range []time.Duration{0, 59 * time.Second, 601 * time.Second} {
		if _, err := db.ReserveModelCallWithin(ctx, a.ID, a.PodUID, 1, modelTestPayload, window); err == nil {
			t.Fatal("window accepted", window)
		}
	}
	if calls, _, _ := modelCount(t, db, b.WorkItem); calls != 0 {
		t.Fatal("refused window wrote the ledger")
	}
	started := time.Now()
	d, err := db.ReserveModelCallWithin(ctx, a.ID, a.PodUID, 1, modelTestPayload, 300*time.Second)
	if err != nil || d.Reason != "reserved" {
		t.Fatalf("reserve: %+v %v", d, err)
	}
	var expires time.Time
	if err = db.Pool.QueryRow(`SELECT expires_at FROM model_calls WHERE allocation_id=$1 AND sequence=1`, a.ID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if expires.Before(started.Add(295*time.Second)) || expires.After(time.Now().Add(301*time.Second)) {
		t.Fatal("call window not applied", expires.Sub(started))
	}
}
