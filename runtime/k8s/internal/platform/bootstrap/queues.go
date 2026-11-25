package bootstrap

import (
	"context"
	"log"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/metrics"
)

// QueueSampleInterval is how often queue depth is read from the store.
const QueueSampleInterval = 30 * time.Second

// QueueReader is the bounded store read behind ai_sdlc_queue_depth.
type QueueReader func(ctx context.Context, component, stage string) (int64, error)

// QueueSampler records each queue's depth. A queue that cannot be read is
// dropped from the metric rather than left stale, and the failure is logged
// once per change of state, never on every sample, and never with its cause.
type QueueSampler struct {
	Read      QueueReader
	Component string
	Log       func(string)
	failing   map[string]bool
}

// Sample reads every stage once, sequentially.
func (s *QueueSampler) Sample(ctx context.Context, stages ...string) {
	if s.failing == nil {
		s.failing = map[string]bool{}
	}
	for _, stage := range stages {
		if ctx.Err() != nil {
			return
		}
		depth, err := s.Read(ctx, s.Component, stage)
		failed := err != nil
		if failed != s.failing[stage] && s.Log != nil {
			if failed {
				s.Log("queue depth unavailable: " + s.Component + " " + stage)
			} else {
				s.Log("queue depth restored: " + s.Component + " " + stage)
			}
		}
		s.failing[stage] = failed
		if failed {
			metrics.ForgetQueueDepth(s.Component, stage)
			continue
		}
		metrics.SetQueueDepth(s.Component, stage, depth)
	}
}

// StartQueueSampler samples the component's own queues every
// QueueSampleInterval, on its own sequential loop so a slow read never delays
// a reconciler. Call the returned stop function before closing the database.
func (c Component) StartQueueSampler(stages ...string) func() {
	sampler := &QueueSampler{Read: c.DB.QueueDepth, Component: c.Name, Log: func(line string) { log.Print(line) }}
	return Periodic(QueueSampleInterval, func(ctx context.Context) { sampler.Sample(ctx, stages...) })
}
