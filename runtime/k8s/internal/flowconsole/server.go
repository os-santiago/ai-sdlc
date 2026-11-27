package flowconsole

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/flowevent"
)

//go:embed ui/*
var uiFS embed.FS

// SnapshotVersion identifies the JSON snapshot document.
const SnapshotVersion = "ai-sdlc.flow-snapshot/v1"

// Server serves the read-only console. It accepts only GET and HEAD.
type Server struct {
	Hub *Hub
	// Replay bounds the history sent to a new SSE client without a cursor.
	Replay int
	// MaxStreams bounds concurrent SSE clients.
	MaxStreams int
	// Heartbeat is the SSE keep-alive interval.
	Heartbeat time.Duration
	// WriteTimeout bounds each SSE write batch, not the stream.
	WriteTimeout time.Duration
	// Ready reports whether the source has produced a successful read.
	Ready func() bool
	// MaxSnapshots bounds concurrent /api/snapshot requests.
	MaxSnapshots int

	streams   chan struct{}
	snapshots chan struct{}
}

// Snapshot pages and SSE write batches hold at most these many events.
const (
	DefaultSnapshotPage = 1000
	MaxSnapshotPage     = 5000
	streamBatch         = 1000
)

// Snapshot is one page of the JSON document served at /api/snapshot:
// retained events after the requested cursor, oldest first. Next is the
// cursor to request the following page with; More says one exists. Cursor
// is the newest cursor the hub had assigned at the time of the read.
type Snapshot struct {
	Version string            `json:"version"`
	Cursor  uint64            `json:"cursor"`
	Next    uint64            `json:"next"`
	More    bool              `json:"more"`
	Gap     bool              `json:"gap,omitempty"`
	Status  Status            `json:"status"`
	Events  []SnapshotElement `json:"events"`
}

// SnapshotElement is one event with its cursor.
type SnapshotElement struct {
	Cursor uint64          `json:"cursor"`
	Event  flowevent.Event `json:"event"`
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	if s.Replay <= 0 {
		s.Replay = 5000
	}
	if s.MaxStreams <= 0 {
		s.MaxStreams = 32
	}
	if s.Heartbeat <= 0 {
		s.Heartbeat = 15 * time.Second
	}
	if s.WriteTimeout <= 0 {
		s.WriteTimeout = 10 * time.Second
	}
	if s.MaxSnapshots <= 0 {
		s.MaxSnapshots = 4
	}
	s.streams = make(chan struct{}, s.MaxStreams)
	s.snapshots = make(chan struct{}, s.MaxSnapshots)
	static, _ := fs.Sub(uiFS, "ui")
	files := http.FileServer(http.FS(static))
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.Ready != nil && !s.Ready() {
			// The reason is the hub's fixed problem vocabulary, never driver text.
			reason := "source not ready"
			if s.Hub != nil && s.Hub.Status().Problem == ProblemConnecting {
				reason = "journal database not reachable yet; retrying"
			}
			http.Error(w, reason, http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("/events", s.events)
	mux.HandleFunc("/api/snapshot", s.snapshot)
	mux.HandleFunc("/api/contract", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"version": flowevent.Version, "stages": flowevent.Stages,
			"kinds": []string{flowevent.KindStarted, flowevent.KindProgressed, flowevent.KindCompleted, flowevent.KindFailed, flowevent.KindStoppedSafely,
				flowevent.KindWaiting, flowevent.KindRetried, flowevent.KindQuarantined, flowevent.KindDegraded, flowevent.KindBlocked}})
	})
	mux.Handle("/", files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			h.Set("Allow", "GET, HEAD")
			http.Error(w, "read-only console", http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/app.") && r.URL.Path != "/" && r.URL.Path != "/index.html" {
			h.Set("Cache-Control", "no-store")
		}
		mux.ServeHTTP(w, r)
	})
}

func parseCursor(r *http.Request) (uint64, bool, error) {
	raw := r.URL.Query().Get("since")
	if raw == "" {
		raw = r.Header.Get("Last-Event-ID")
	}
	if raw == "" {
		return 0, false, nil
	}
	if len(raw) > 20 {
		return 0, false, fmt.Errorf("invalid cursor")
	}
	cursor, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("invalid cursor")
	}
	return cursor, true, nil
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	cursor, ok, err := parseCursor(r)
	if err != nil {
		http.Error(w, "invalid cursor", http.StatusBadRequest)
		return
	}
	limit := DefaultSnapshotPage
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > MaxSnapshotPage || len(raw) > 5 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		limit = n
	}
	select {
	case s.snapshots <- struct{}{}:
		defer func() { <-s.snapshots }()
	default:
		http.Error(w, "too many snapshot requests", http.StatusServiceUnavailable)
		return
	}
	// One hub read: the page and the cursor it ends at are consistent.
	entries, current, gap := s.Hub.Since(cursor, limit)
	if !ok {
		gap = false
	}
	doc := Snapshot{Version: SnapshotVersion, Cursor: current, Next: cursor, Gap: gap, Status: s.Hub.Status(), Events: make([]SnapshotElement, 0, len(entries))}
	for _, e := range entries {
		doc.Events = append(doc.Events, SnapshotElement{Cursor: e.Cursor, Event: e.Event})
		doc.Next = e.Cursor
	}
	doc.More = doc.Next < current
	w.Header().Set("Content-Type", "application/json")
	// A client that stops reading must not hold a snapshot slot.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.WriteTimeout))
	json.NewEncoder(w).Encode(doc)
}

// events streams Server-Sent Events: first the replay (the newest Replay
// entries, or every retained entry after the client's cursor), then a
// "ready" marker, then live entries as the hub receives them, batched in
// "flows" messages whose SSE id is their last cursor, so EventSource resumes
// with Last-Event-ID after a reconnect without duplicates. A "status" event
// reports the source state; "gap" says entries after the cursor were evicted.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	cursor, resumed, err := parseCursor(r)
	if err != nil {
		http.Error(w, "invalid cursor", http.StatusBadRequest)
		return
	}
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		http.Error(w, "too many streams", http.StatusServiceUnavailable)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	updates, cancel := s.Hub.Subscribe()
	defer cancel()

	// Each write batch gets its own deadline: a client that stops reading
	// without closing cannot hold a stream slot, while a healthy connection
	// stays open indefinitely.
	control := http.NewResponseController(w)
	send := func(write func() error) bool {
		if control.SetWriteDeadline(time.Now().Add(s.WriteTimeout)) != nil {
			return false
		}
		return write() == nil && control.Flush() == nil
	}

	// The replay is written in batches of at most streamBatch events, each
	// with its own write deadline: a resumed client may be far behind.
	var batch []Entry
	var gap bool
	if resumed {
		batch, _, gap = s.Hub.Since(cursor, streamBatch)
	} else {
		batch = s.Hub.Tail(s.Replay)
	}
	if !send(func() error {
		if gap {
			if err := writeJSON(w, "gap", "", map[string]uint64{"after": cursor}); err != nil {
				return err
			}
		}
		_, err := fmt.Fprint(w, "retry: 3000\n\n")
		return err
	}) {
		return
	}
	for len(batch) > 0 {
		chunk := batch
		if len(chunk) > streamBatch {
			chunk = chunk[:streamBatch]
		}
		if !send(func() error { return writeEntries(w, chunk, &cursor) }) {
			return
		}
		batch = batch[len(chunk):]
		if resumed && len(batch) == 0 {
			batch, _, _ = s.Hub.Since(cursor, streamBatch)
		}
	}
	status := s.Hub.Status()
	if !send(func() error {
		if err := writeJSON(w, "status", "", status); err != nil {
			return err
		}
		return writeJSON(w, "ready", "", map[string]uint64{"cursor": cursor})
	}) {
		return
	}

	heartbeat := time.NewTicker(s.Heartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if !send(func() error { _, err := fmt.Fprint(w, ": keep-alive\n\n"); return err }) {
				return
			}
		case <-updates:
			ok := send(func() error {
				for {
					// A live client that fell behind the retained history is
					// told so before it receives the entries that remain.
					next, _, evicted := s.Hub.Since(cursor, 1000)
					if evicted {
						if err := writeJSON(w, "gap", "", map[string]uint64{"after": cursor}); err != nil {
							return err
						}
					}
					if len(next) == 0 {
						break
					}
					if err := writeEntries(w, next, &cursor); err != nil {
						return err
					}
				}
				if next := s.Hub.Status(); next != status {
					status = next
					return writeJSON(w, "status", "", status)
				}
				return nil
			})
			if !ok {
				return
			}
		}
	}
}

// sseBatch bounds the events carried by one "flows" message. Browsers
// dispatch every SSE message as a separate task, so one message per event
// made a 20,000-event replay take many seconds; batches keep it well under
// one.
const sseBatch = 250

// writeEntries writes entries as "flows" messages: a JSON array of
// {cursor, event} elements (the snapshot's element shape). The SSE id is the
// last cursor of the message, so Last-Event-ID resumes after it.
func writeEntries(w http.ResponseWriter, entries []Entry, cursor *uint64) error {
	for len(entries) > 0 {
		n := min(len(entries), sseBatch)
		batch := make([]SnapshotElement, n)
		for i, e := range entries[:n] {
			batch[i] = SnapshotElement{Cursor: e.Cursor, Event: e.Event}
		}
		if err := writeJSON(w, "flows", strconv.FormatUint(entries[n-1].Cursor, 10), batch); err != nil {
			return err
		}
		*cursor = entries[n-1].Cursor
		entries = entries[n:]
	}
	return nil
}

func writeJSON(w http.ResponseWriter, event, id string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var b strings.Builder
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	b.WriteString("event: " + event + "\ndata: ")
	b.Write(raw)
	b.WriteString("\n\n")
	_, err = w.Write([]byte(b.String()))
	return err
}
