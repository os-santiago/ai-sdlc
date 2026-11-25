package bootstrap

import (
	"context"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/metrics"
)

// QueueSampleInterval is how often queue depth is read from the store.
const QueueSampleInterval = 30 * time.Second

// QueueReader is the bounded store read behind ai_sdlc_queue_depth.
type QueueReader func(ctx context.Context, component, stage string) (int64, error)

// SampleQueues records each queue's depth once. A queue that cannot be read
// is dropped from the metric rather than left stale.
func SampleQueues(ctx context.Context, read QueueReader, component string, stages ...string) {
	for _, stage := range stages {
		if ctx.Err() != nil {
			return
		}
		depth, err := read(ctx, component, stage)
		if err != nil {
			metrics.ForgetQueueDepth(component, stage)
			continue
		}
		metrics.SetQueueDepth(component, stage, depth)
	}
}

// StartQueueSampler samples the component's own queues every
// QueueSampleInterval, on its own sequential loop so a slow read never delays
// a reconciler. Call the returned stop function before closing the database.
func (c Component) StartQueueSampler(stages ...string) func() {
	return Periodic(QueueSampleInterval, func(ctx context.Context) {
		SampleQueues(ctx, c.DB.QueueDepth, c.Name, stages...)
	})
}
