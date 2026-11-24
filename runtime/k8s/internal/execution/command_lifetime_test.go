package execution

import (
	"testing"
	"time"
)

// The default generation budget keeps the original 20-minute command; a
// larger reviewed budget extends it second for second, bounded at 900 s.
func TestCommandLifetimeKeepsHandoffRoomForLongerGeneration(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		want    time.Duration
	}{{1, 20 * time.Minute}, {180, 20 * time.Minute}, {600, 20 * time.Minute}, {750, 22*time.Minute + 30*time.Second}, {900, 25 * time.Minute}, {5000, 25 * time.Minute}} {
		got := CommandLifetime(tc.seconds)
		if got != tc.want {
			t.Fatal("lifetime", tc.seconds, got)
		}
		// Queueing, preparation and handoff keep at least the original 480 s
		// beyond the lifecycle claim (maxSeconds + 120 s, at most 1,020 s).
		if claim := time.Duration(min(tc.seconds, 900)+120) * time.Second; got-claim < 480*time.Second {
			t.Fatal("handoff room", tc.seconds, got-claim)
		}
		if got > time.Hour {
			t.Fatal("beyond the command verification bound")
		}
	}
}
