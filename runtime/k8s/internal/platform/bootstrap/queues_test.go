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
	var logged []string
	sampler := &QueueSampler{Read: read, Component: "verifier", Log: func(line string) { logged = append(logged, line) }}
	sampler.Sample(context.Background(), "outbox", "verification")
	for stage, want := range depths {
		if got, ok := metrics.Default.Sample("ai_sdlc_queue_depth", "verifier", stage); !ok || got != float64(want) {
			t.Fatal(stage, got, ok)
		}
	}
	if len(logged) != 0 {
		t.Fatal("healthy samples logged", logged)
	}
	failing = true
	for i := 0; i < 3; i++ {
		sampler.Sample(context.Background(), "outbox", "verification")
	}
	if len(logged) != 1 || logged[0] != "queue depth unavailable: verifier verification" {
		t.Fatal("failure not logged exactly once", logged)
	}
	if _, ok := metrics.Default.Sample("ai_sdlc_queue_depth", "verifier", "verification"); ok {
		t.Fatal("unreadable queue kept a stale depth")
	}
	if got, _ := metrics.Default.Sample("ai_sdlc_queue_depth", "verifier", "outbox"); got != 4 {
		t.Fatal("one failed read dropped another queue")
	}
	failing = false
	sampler.Sample(context.Background(), "outbox", "verification")
	sampler.Sample(context.Background(), "outbox", "verification")
	if len(logged) != 2 || logged[1] != "queue depth restored: verifier verification" {
		t.Fatal("recovery not logged exactly once", logged)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	(&QueueSampler{Read: func(context.Context, string, string) (int64, error) { t.Fatal("read after cancel"); return 0, nil }, Component: "verifier"}).Sample(cancelled, "outbox")
}
