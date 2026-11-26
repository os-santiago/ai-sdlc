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
	// Ready reports whether the source has produced a successful read.
	Ready func() bool

	streams chan struct{}
}

// Snapshot is the JSON document served at /api/snapshot.
type Snapshot struct {
	Version string            `json:"version"`
	Cursor  uint64            `json:"cursor"`
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
	s.streams = make(chan struct{}, s.MaxStreams)
	static, _ := fs.Sub(uiFS, "ui")
	files := http.FileServer(http.FS(static))
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.Ready != nil && !s.Ready() {
			http.Error(w, "source not ready", http.StatusServiceUnavailable)
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
	var entries []Entry
	var current uint64
	var gap bool
	if ok {
		entries, current, gap = s.Hub.Since(cursor, 0)
	} else {
		entries = s.Hub.Tail(0)
		_, current, _ = s.Hub.Since(^uint64(0), 1)
	}
	doc := Snapshot{Version: SnapshotVersion, Cursor: current, Gap: gap, Status: s.Hub.Status(), Events: make([]SnapshotElement, 0, len(entries))}
	for _, e := range entries {
		doc.Events = append(doc.Events, SnapshotElement{Cursor: e.Cursor, Event: e.Event})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(doc)
}

// events streams Server-Sent Events: first the replay (the newest Replay
// entries, or every retained entry after the client's cursor), then a
// "ready" marker, then live entries as the hub receives them. Each event
// carries its cursor as the SSE id, so EventSource resumes with
// Last-Event-ID after a reconnect without duplicates. A "status" event
// reports the source state; "gap" says entries after the cursor were evicted.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	cursor, resumed, err := parseCursor(r)
	if err != nil {
		http.Error(w, "invalid cursor", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
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

	var batch []Entry
	if resumed {
		var gap bool
		batch, _, gap = s.Hub.Since(cursor, 0)
		if gap {
			fmt.Fprint(w, "event: gap\ndata: {}\n\n")
		}
	} else {
		batch = s.Hub.Tail(s.Replay)
	}
	fmt.Fprint(w, "retry: 3000\n\n")
	if err := writeEntries(w, batch, &cursor); err != nil {
		return
	}
	status := s.Hub.Status()
	writeJSON(w, "status", "", status)
	writeJSON(w, "ready", "", map[string]uint64{"cursor": cursor})
	flusher.Flush()

	heartbeat := time.NewTicker(s.Heartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-updates:
			for {
				batch, _, _ = s.Hub.Since(cursor, 1000)
				if len(batch) == 0 {
					break
				}
				if err := writeEntries(w, batch, &cursor); err != nil {
					return
				}
			}
			if next := s.Hub.Status(); next != status {
				status = next
				writeJSON(w, "status", "", status)
			}
			flusher.Flush()
		}
	}
}

func writeEntries(w http.ResponseWriter, entries []Entry, cursor *uint64) error {
	for _, e := range entries {
		if err := writeJSON(w, "flow", strconv.FormatUint(e.Cursor, 10), e.Event); err != nil {
			return err
		}
		*cursor = e.Cursor
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
