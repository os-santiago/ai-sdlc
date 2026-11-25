package bootstrap

import (
	"context"
	"errors"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/metrics"
)

func TestSampleQueuesRecordsDepthAndDropsUnreadableQueues(t *testing.T) {
	depths := map[string]int64{"outbox": 4, "verification": 2}
	failing := false
	read := func(_ context.Context, component, stage string) (int64, error) {
		if component != "verifier" {
			t.Fatal("sampled another component", component)
		}
		if failing && stage == "verification" {
			return 0, errors.New("database unavailable")
		}
		return depths[stage], nil
	}
	SampleQueues(context.Background(), read, "verifier", "outbox", "verification")
	for stage, want := range depths {
		if got, ok := metrics.Default.Sample("ai_sdlc_queue_depth", "verifier", stage); !ok || got != float64(want) {
			t.Fatal(stage, got, ok)
		}
	}
	failing = true
	SampleQueues(context.Background(), read, "verifier", "outbox", "verification")
	if _, ok := metrics.Default.Sample("ai_sdlc_queue_depth", "verifier", "verification"); ok {
		t.Fatal("unreadable queue kept a stale depth")
	}
	if got, _ := metrics.Default.Sample("ai_sdlc_queue_depth", "verifier", "outbox"); got != 4 {
		t.Fatal("one failed read dropped another queue")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	SampleQueues(cancelled, func(context.Context, string, string) (int64, error) { t.Fatal("read after cancel"); return 0, nil }, "verifier", "outbox")
}
