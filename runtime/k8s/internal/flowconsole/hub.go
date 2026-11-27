package flowconsole

import (
	"sync"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/flowevent"
)

// Entry is an event with its cursor: a process-local sequence number
// assigned on first observation. Cursors increase strictly; an SSE client
// resumes with Last-Event-ID or ?since= and receives every later entry still
// retained.
type Entry struct {
	Cursor uint64
	Event  flowevent.Event
}

// Status describes the event source for the UI banner. Source errors are a
// fixed vocabulary; driver or SQL text is never exposed.
type Status struct {
	Source     string    `json:"source"`
	Healthy    bool      `json:"healthy"`
	Problem    string    `json:"problem,omitempty"`
	ObservedAt time.Time `json:"observedAt,omitempty"`
	Truncated  bool      `json:"truncated,omitempty"`
	Window     string    `json:"window,omitempty"`
}

// ProblemConnecting is the status problem while the console has not yet
// reached the journal database at startup (for example on a fresh install,
// before PostgreSQL or the console's role is ready). It is retried with
// bounded backoff; readiness stays false until the first successful read.
const ProblemConnecting = "journal-connecting"

// Hub retains at most capacity entries, de-duplicates by event id, and wakes
// subscribers on every append or status change.
type Hub struct {
	mu          sync.Mutex
	capacity    int
	entries     []Entry
	cursor      uint64
	seen        map[string]time.Time
	status      Status
	subscribers map[chan struct{}]struct{}
}

// NewHub creates a hub. capacity bounds memory and replay size.
func NewHub(capacity int, source string) *Hub {
	if capacity < 1 {
		capacity = 1
	}
	return &Hub{capacity: capacity, seen: map[string]time.Time{}, subscribers: map[chan struct{}]struct{}{}, status: Status{Source: source}}
}

// Publish appends events not seen before, in the given order, and returns
// how many were new. Events must already be valid.
func (h *Hub) Publish(events []flowevent.Event) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	added := 0
	for _, e := range events {
		if _, ok := h.seen[e.ID]; ok {
			continue
		}
		h.cursor++
		h.seen[e.ID] = e.Time
		h.entries = append(h.entries, Entry{Cursor: h.cursor, Event: e})
		added++
	}
	if over := len(h.entries) - h.capacity; over > 0 {
		h.entries = append([]Entry(nil), h.entries[over:]...)
	}
	if added > 0 {
		h.notify()
	}
	return added
}

// Forget drops de-duplication memory for events older than cutoff, so the
// id set stays bounded by the collector window, but never for an id in
// current: the ids the latest poll derived. An event time can lie before the
// window while its row is still selected (for example a work item created
// long ago that changed today); forgetting it would re-publish it with a new
// cursor on every poll and push real history out of the buffer.
func (h *Hub) Forget(cutoff time.Time, current map[string]bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, at := range h.seen {
		if at.Before(cutoff) && !current[id] {
			delete(h.seen, id)
		}
	}
}

// SetStatus records the source state and wakes subscribers when it changes.
func (h *Hub) SetStatus(s Status) {
	h.mu.Lock()
	defer h.mu.Unlock()
	changed := s.Healthy != h.status.Healthy || s.Problem != h.status.Problem || s.Truncated != h.status.Truncated
	h.status = s
	if changed {
		h.notify()
	}
}

// Status returns the current source state.
func (h *Hub) Status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

// Since returns retained entries after cursor (at most limit), the current
// cursor, and whether entries after cursor were already evicted (a gap).
func (h *Hub) Since(cursor uint64, limit int) ([]Entry, uint64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	gap := len(h.entries) > 0 && cursor+1 < h.entries[0].Cursor && cursor > 0
	start := len(h.entries)
	for i, e := range h.entries {
		if e.Cursor > cursor {
			start = i
			break
		}
	}
	out := h.entries[start:]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return append([]Entry(nil), out...), h.cursor, gap
}

// Tail returns the newest n retained entries (the replay of recent history).
func (h *Hub) Tail(n int) []Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	start := 0
	if n > 0 && len(h.entries) > n {
		start = len(h.entries) - n
	}
	return append([]Entry(nil), h.entries[start:]...)
}

// Subscribe returns a channel that receives a signal (coalesced) after each
// change, and a function that unsubscribes.
func (h *Hub) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subscribers, ch)
		h.mu.Unlock()
	}
}

func (h *Hub) notify() {
	for ch := range h.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
