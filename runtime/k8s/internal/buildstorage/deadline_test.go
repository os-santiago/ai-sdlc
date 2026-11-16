package buildstorage

import (
	"testing"
	"time"
)

func TestQueuedAuthorityMustCoverWholePhase(t *testing.T) {
	now := time.Now()
	for _, phase := range []string{"build", "publish"} {
		if CheckDeadline(phase, now.Add(20*time.Minute).UnixMilli(), now) != nil {
			t.Fatal("fresh authority denied")
		}
		for _, delta := range []time.Duration{-time.Second, 0, time.Minute, 31 * time.Minute} {
			if CheckDeadline(phase, now.Add(delta).UnixMilli(), now) == nil {
				t.Fatal("expired, insufficient or unbounded authority accepted")
			}
		}
	}
	if CheckDeadline("unknown", now.Add(20*time.Minute).UnixMilli(), now) == nil {
		t.Fatal("unknown phase accepted")
	}
}
