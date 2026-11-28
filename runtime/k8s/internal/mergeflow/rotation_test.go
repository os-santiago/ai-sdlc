package mergeflow

import (
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func pub(c string) string { return "publication-" + strings.Repeat(c, 64) }

// A publication observed goes to the back; a parked one is left out until its
// park ends; a nil rotation selects in publication order.
func TestRotationOrdersLeastRecentlyObservedFirst(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	r := &Rotation{now: func() time.Time { return now }}
	if got := (*Rotation)(nil).snapshot(); len(got.Recent) != 0 || len(got.Parked) != 0 {
		t.Fatal("nil rotation", got)
	}
	r.observed(pub("a"), 0, false, nil)
	now = now.Add(time.Minute)
	r.observed(pub("b"), 0, false, nil)
	now = now.Add(time.Minute)
	r.observed(pub("c"), ParkClosed, true, nil)
	got := r.snapshot()
	if strings.Join(got.Recent, ",") != pub("a")+","+pub("b") || strings.Join(got.Parked, ",") != pub("c") {
		t.Fatal("order", got)
	}
	now = now.Add(time.Minute)
	r.observed(pub("a"), 0, false, nil)
	if got = r.snapshot(); strings.Join(got.Recent, ",") != pub("b")+","+pub("a") {
		t.Fatal("observed publication not moved to the back", got)
	}
	now = now.Add(ParkClosed)
	if got = r.snapshot(); len(got.Parked) != 0 || got.Recent[len(got.Recent)-1] != pub("a") || len(got.Recent) != 3 {
		t.Fatal("park did not end", got)
	}
	now = now.Add(rotationMemory + time.Minute)
	if got = r.snapshot(); len(got.Recent) != 0 || len(r.entries) != 0 {
		t.Fatal("old entries kept", got)
	}
}

// The rotation never grows past what the store accepts, and every snapshot is
// a valid store rotation.
func TestRotationIsBounded(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	r := &Rotation{now: func() time.Time { return now }}
	hex := "0123456789abcdef"
	for i := 0; i < store.MaxMergeRotation+40; i++ {
		now = now.Add(time.Second)
		id := "publication-" + strings.Repeat("0", 60) + string(hex[i/256%16]) + string(hex[i/16%16]) + string(hex[i%16]) + "0"
		r.observed(id, 0, false, nil)
	}
	got := r.snapshot()
	if len(r.entries) != store.MaxMergeRotation || len(got.Recent) != store.MaxMergeRotation {
		t.Fatal("rotation size", len(r.entries))
	}
	// The newest entries survived, the least recently observed were dropped.
	if !strings.HasSuffix(got.Recent[len(got.Recent)-1], "1270") {
		t.Fatal("newest entry dropped", got.Recent[len(got.Recent)-1])
	}
	// A table full of parked entries leaves a new one untracked.
	p := &Rotation{now: func() time.Time { return now }}
	for i := 0; i < store.MaxMergeRotation; i++ {
		p.observed("publication-"+strings.Repeat("1", 60)+string(hex[i/256%16])+string(hex[i/16%16])+string(hex[i%16])+"0", ParkClosed, true, nil)
	}
	p.observed(pub("e"), 0, false, nil)
	if _, tracked := p.entries[pub("e")]; tracked || len(p.entries) != store.MaxMergeRotation {
		t.Fatal("parked entries evicted")
	}
}

// Wait counts follow each publication's latest gate observation: a poll that
// could not observe the gate keeps the last wait, a ready or closed gate
// clears it, and an old wait stops counting.
func TestRotationSummarizesLatestWaits(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	r := &Rotation{now: func() time.Time { return now }}
	threads := &Wait{State: "merge-waiting-for-review-threads", Reasons: []string{"review-threads-unresolved", "native-merge-not-ready"}, UnresolvedThreads: 2}
	checks := &Wait{State: "merge-waiting-for-checks", Reasons: []string{"checks-not-passed", "review-threads-unresolved"}, UnresolvedThreads: 1}
	r.observed(pub("a"), 0, true, threads)
	r.observed(pub("b"), 0, true, checks)
	threads.Reasons[0] = "mutated"
	got := r.Waits()
	if got.Reasons["review-threads-unresolved"] != 2 || got.Reasons["checks-not-passed"] != 1 || got.Reasons["native-merge-not-ready"] != 1 || got.UnresolvedThreads != 3 {
		t.Fatal("summary", got)
	}
	r.observed(pub("a"), 0, false, nil) // gate unavailable: wait kept
	if r.Waits().UnresolvedThreads != 3 {
		t.Fatal("unobserved gate cleared a wait")
	}
	r.observed(pub("b"), 0, true, nil) // ready or closed: cleared
	if got = r.Waits(); got.Reasons["checks-not-passed"] != 0 || got.UnresolvedThreads != 2 {
		t.Fatal("observed gate kept a wait", got)
	}
	now = now.Add(WaitMemory + time.Second)
	if got = r.Waits(); len(got.Reasons) != 0 || got.UnresolvedThreads != 0 {
		t.Fatal("old wait counted", got)
	}
	if got = (*Rotation)(nil).Waits(); len(got.Reasons) != 0 {
		t.Fatal("nil rotation", got)
	}
}
