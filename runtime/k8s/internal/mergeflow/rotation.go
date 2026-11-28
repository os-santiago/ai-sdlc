package mergeflow

import (
	"sort"
	"sync"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// ParkClosed is how long a publication whose pull request was observed closed
// (or merged outside the controller) is not selected again. After it the
// publication is observed once more, so a reopened pull request resumes.
const ParkClosed = 30 * time.Minute

// Rotation remembers, in the release manager's memory only, when each
// publication was last observed and which are parked, so selection cannot be
// held by one publication whose gate stays unready. It holds publication IDs
// and times only, is bounded by store.MaxMergeRotation and forgets entries a
// day old. Losing it on restart is safe: selection falls back to publication
// order and each publication is observed again. The zero value is ready.
type Rotation struct {
	mu      sync.Mutex
	entries map[string]rotationEntry
	now     func() time.Time
}

type rotationEntry struct {
	observed, parkedUntil time.Time
}

const rotationMemory = 24 * time.Hour

// NewRotation returns a rotation on the given clock (nil uses time.Now).
func NewRotation(clock func() time.Time) *Rotation { return &Rotation{now: clock} }

func (r *Rotation) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// snapshot returns the selection order: observed publications least recently
// first, and those still parked.
func (r *Rotation) snapshot() store.MergeRotation {
	if r == nil {
		return store.MergeRotation{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	type item struct {
		id string
		at time.Time
	}
	var recent []item
	out := store.MergeRotation{Recent: []string{}, Parked: []string{}}
	for id, e := range r.entries {
		if now.Sub(e.observed) > rotationMemory {
			delete(r.entries, id)
			continue
		}
		if e.parkedUntil.After(now) {
			out.Parked = append(out.Parked, id)
			continue
		}
		recent = append(recent, item{id, e.observed})
	}
	sort.Slice(recent, func(i, j int) bool {
		if !recent[i].at.Equal(recent[j].at) {
			return recent[i].at.Before(recent[j].at)
		}
		return recent[i].id < recent[j].id
	})
	for _, it := range recent {
		out.Recent = append(out.Recent, it.id)
	}
	sort.Strings(out.Parked)
	return out
}

// observed records that a publication was just observed; park also keeps it
// out of selection for that long.
func (r *Rotation) observed(id string, park time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = map[string]rotationEntry{}
	}
	now := r.clock()
	if _, known := r.entries[id]; !known && len(r.entries) >= store.MaxMergeRotation {
		// Full: forget the least recently observed unparked entry. A full
		// table of parked entries leaves the new one untracked, so it is
		// selected in publication order as before.
		oldest, at := "", now
		for k, e := range r.entries {
			if !e.parkedUntil.After(now) && e.observed.Before(at) {
				oldest, at = k, e.observed
			}
		}
		if oldest == "" {
			return
		}
		delete(r.entries, oldest)
	}
	e := rotationEntry{observed: now}
	if park > 0 {
		e.parkedUntil = now.Add(park)
	}
	r.entries[id] = e
}
