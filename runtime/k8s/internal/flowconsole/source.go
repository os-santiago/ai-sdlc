package flowconsole

import (
	"context"
	"io"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/flowevent"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// JournalReader is the read side of the collector; store.ReadFlowJournal in
// production, a stub in tests.
type JournalReader func(ctx context.Context, since time.Time, limit int) (store.FlowJournal, error)

// Collector polls the journal on a short interval. Each poll re-reads the
// bounded window (work that started within Window), derives events and
// publishes only ids not published before: the journal holds mutable state
// rows, so a stage's later facts appear as new events on a later poll.
type Collector struct {
	Hub      *Hub
	Read     JournalReader
	Interval time.Duration
	Window   time.Duration
	Limit    int
	Options  DeriveOptions
}

// Poll performs one read and publish. It returns the number of new events.
func (c Collector) Poll(ctx context.Context, now time.Time) (int, error) {
	j, err := c.Read(ctx, now.Add(-c.Window), c.Limit)
	if err != nil {
		c.Hub.SetStatus(Status{Source: "journal", Healthy: false, Problem: "journal-unavailable", ObservedAt: c.Hub.Status().ObservedAt, Window: c.Window.String()})
		return 0, err
	}
	added := c.Hub.Publish(Derive(j, c.Options))
	c.Hub.Forget(j.ObservedAt.Add(-c.Window - time.Hour))
	problem := ""
	if j.Truncated {
		problem = "window-truncated"
	}
	c.Hub.SetStatus(Status{Source: "journal", Healthy: true, Problem: problem, ObservedAt: j.ObservedAt, Truncated: j.Truncated, Window: c.Window.String()})
	return added, nil
}

// Fixture replays recorded events. Times are shifted so that the recording
// ends Live after start: events up to now are history, the rest are released
// as wall time reaches them, which exercises the live path of the UI without
// a cluster. Live zero publishes everything at once at its recorded time.
type Fixture struct {
	Hub    *Hub
	Events []flowevent.Event
	Live   time.Duration
}

// LoadFixture decodes and validates a fixture stream.
func LoadFixture(r io.Reader) ([]flowevent.Event, error) { return flowevent.Decode(r) }

// Run publishes the fixture until ctx ends.
func (f Fixture) Run(ctx context.Context, start time.Time) {
	events := append([]flowevent.Event(nil), f.Events...)
	if f.Live > 0 && len(events) > 0 {
		shift := start.Add(f.Live).Sub(events[len(events)-1].Time)
		for i := range events {
			events[i].Time = events[i].Time.Add(shift).Truncate(time.Millisecond)
		}
	}
	f.Hub.SetStatus(Status{Source: "fixture", Healthy: true, ObservedAt: start})
	next := 0
	release := func(now time.Time) {
		end := next
		for end < len(events) && (f.Live == 0 || !events[end].Time.After(now)) {
			end++
		}
		if end > next {
			f.Hub.Publish(events[next:end])
			next = end
		}
		f.Hub.SetStatus(Status{Source: "fixture", Healthy: true, ObservedAt: now})
	}
	release(start)
	if f.Live == 0 {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			release(now)
		}
	}
}
