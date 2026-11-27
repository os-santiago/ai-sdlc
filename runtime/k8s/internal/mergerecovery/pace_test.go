package mergerecovery

import (
	"testing"
	"time"
)

// Every outcome that leaves nothing to observe clears the effect's pacing,
// including an effect another replica or an operator resolved first; every
// other outcome backs off from 30 s, doubling, up to 15 min.
func TestPacingClearsOnlyWhenNothingIsLeftToObserve(t *testing.T) {
	now := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	for _, outcome := range []string{Completed, NotSent, Resolved} {
		r := &Reconciler{Now: func() time.Time { return now }}
		r.pace("effect", Unavailable)
		if r.failures["effect"] != 1 || !r.next["effect"].Equal(now.Add(firstRetry)) {
			t.Fatal("first failure not paced", r.failures, r.next)
		}
		r.pace("effect", outcome)
		if _, paced := r.next["effect"]; paced || r.failures["effect"] != 0 || len(r.failures) != 0 {
			t.Fatal("pacing kept after a final outcome", outcome, r.failures, r.next)
		}
	}
	r := &Reconciler{Now: func() time.Time { return now }}
	for i, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		outcome := []string{Mismatch, Unavailable, Unplanned}[i%3]
		r.pace("effect", outcome)
		if got := r.next["effect"].Sub(now); got != want {
			t.Fatal("backoff", i, outcome, got, want)
		}
	}
}
