package flowconsole

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/flowevent"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func event(n int) flowevent.Event {
	return flowevent.Event{Version: flowevent.Version, ID: flowevent.NewID("e", fmt.Sprint(n)), Time: time.Date(2026, 11, 25, 12, 0, n, 0, time.UTC),
		Component: "intake", Repository: "example/app", Issue: 7, Stage: "intake", Step: "observed", Kind: flowevent.KindCompleted}
}

type sseMessage struct{ id, event, data string }

// readSSE reads messages until it has n flow events or sees stop. Each event
// of a "flows" batch is expanded into one "flow" message whose id is its
// cursor and whose data is the event; a batch's own SSE id must be the
// cursor of its last event.
func readSSE(t *testing.T, scanner *bufio.Scanner, n int, stop string) []sseMessage {
	t.Helper()
	var out []sseMessage
	var m sseMessage
	flows := 0
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if m.event == "flows" {
				var batch []SnapshotElement
				if err := json.Unmarshal([]byte(m.data), &batch); err != nil || len(batch) == 0 || len(batch) > sseBatch || fmt.Sprint(batch[len(batch)-1].Cursor) != m.id {
					t.Fatal("malformed flows batch", err)
				}
				for _, e := range batch {
					raw, _ := json.Marshal(e.Event)
					out = append(out, sseMessage{id: fmt.Sprint(e.Cursor), event: "flow", data: string(raw)})
					flows++
				}
				if n > 0 && flows >= n {
					return out
				}
			} else if m.event != "" {
				out = append(out, m)
				if m.event == stop {
					return out
				}
			}
			m = sseMessage{}
		case strings.HasPrefix(line, "id: "):
			m.id = line[4:]
		case strings.HasPrefix(line, "event: "):
			m.event = line[7:]
		case strings.HasPrefix(line, "data: "):
			m.data = line[6:]
		}
	}
	t.Fatal("stream ended", scanner.Err())
	return nil
}

func open(t *testing.T, url, lastEventID string) (*bufio.Scanner, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal("not an event stream", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<24)
	return scanner, func() { cancel(); resp.Body.Close() }
}

func TestEventStreamReplaysThenStreamsLiveAndResumesFromCursor(t *testing.T) {
	hub := NewHub(100, "journal")
	hub.Publish([]flowevent.Event{event(1), event(2), event(3)})
	server := httptest.NewServer((&Server{Hub: hub, Replay: 2}).Handler())
	defer server.Close()

	stream, done := open(t, server.URL+"/events", "")
	replay := readSSE(t, stream, 0, "ready")
	var flows []sseMessage
	for _, m := range replay {
		if m.event == "flow" {
			flows = append(flows, m)
		}
	}
	// Replay is bounded to the newest entries, each carrying its cursor.
	if len(flows) != 2 || flows[0].id != "2" || flows[1].id != "3" || replay[len(replay)-1].data != `{"cursor":3}` {
		t.Fatalf("replay %+v", replay)
	}
	var decoded flowevent.Event
	if json.Unmarshal([]byte(flows[1].data), &decoded) != nil || decoded.ID != event(3).ID || decoded.Validate() != nil {
		t.Fatal("replayed event does not round-trip the contract")
	}
	// Live: a re-derived duplicate is not resent; new events follow in order.
	hub.Publish([]flowevent.Event{event(3), event(4), event(5)})
	live := readSSE(t, stream, 2, "")
	if live[len(live)-2].id != "4" || live[len(live)-1].id != "5" {
		t.Fatalf("live %+v", live)
	}
	done()

	// Reconnect with Last-Event-ID: only later entries, no duplicates.
	hub.Publish([]flowevent.Event{event(6)})
	resumed, done2 := open(t, server.URL+"/events", "4")
	defer done2()
	got := readSSE(t, resumed, 0, "ready")
	var ids []string
	for _, m := range got {
		if m.event == "flow" {
			ids = append(ids, m.id)
		}
	}
	if strings.Join(ids, ",") != "5,6" {
		t.Fatalf("resume %v", ids)
	}
}

func TestEventStreamReportsGapAfterEviction(t *testing.T) {
	hub := NewHub(2, "journal")
	hub.Publish([]flowevent.Event{event(1), event(2), event(3), event(4)})
	server := httptest.NewServer((&Server{Hub: hub}).Handler())
	defer server.Close()
	stream, done := open(t, server.URL+"/events?since=1", "")
	defer done()
	got := readSSE(t, stream, 0, "ready")
	if got[0].event != "gap" {
		t.Fatalf("no gap reported %+v", got)
	}
}

func TestLiveStreamReportsGapWhenItFallsBehindRetention(t *testing.T) {
	hub := NewHub(2, "journal")
	hub.Publish([]flowevent.Event{event(1), event(2)})
	server := httptest.NewServer((&Server{Hub: hub}).Handler())
	defer server.Close()
	stream, done := open(t, server.URL+"/events", "")
	defer done()
	readSSE(t, stream, 0, "ready")
	// Three events in one publish evict cursor 3 before the stream reads it.
	hub.Publish([]flowevent.Event{event(3), event(4), event(5)})
	got := readSSE(t, stream, 2, "")
	if got[0].event != "gap" || got[0].data != `{"after":2}` || got[len(got)-2].id != "4" || got[len(got)-1].id != "5" {
		t.Fatalf("live gap %+v", got)
	}
}

func TestSnapshotIsPagedAndConcurrencyBounded(t *testing.T) {
	hub := NewHub(100, "journal")
	var events []flowevent.Event
	for i := 1; i <= 25; i++ {
		events = append(events, event(i))
	}
	hub.Publish(events)
	srv := &Server{Hub: hub, MaxSnapshots: 2}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	get := func(query string) (Snapshot, int) {
		resp, err := http.Get(server.URL + "/api/snapshot" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var snap Snapshot
		json.NewDecoder(resp.Body).Decode(&snap)
		return snap, resp.StatusCode
	}
	var pages []int
	next, more := uint64(0), true
	for more {
		snap, code := get(fmt.Sprintf("?since=%d&limit=10", next))
		if code != 200 || snap.Cursor != 25 {
			t.Fatal("page", code, snap.Cursor)
		}
		pages = append(pages, len(snap.Events))
		next, more = snap.Next, snap.More
	}
	if fmt.Sprint(pages) != "[10 10 5]" || next != 25 {
		t.Fatal("pages", pages, next)
	}
	if snap, _ := get(""); len(snap.Events) != 25 || snap.More {
		t.Fatal("default page", len(snap.Events), snap.More)
	}
	for _, bad := range []string{"?limit=0", "?limit=5001", "?limit=x", "?limit=-1"} {
		if _, code := get(bad); code != http.StatusBadRequest {
			t.Fatal(bad, "accepted", code)
		}
	}
	// With every snapshot slot taken, a further request is refused at once.
	srv.snapshots <- struct{}{}
	srv.snapshots <- struct{}{}
	if _, code := get(""); code != http.StatusServiceUnavailable {
		t.Fatal("snapshot concurrency not bounded", code)
	}
	<-srv.snapshots
	<-srv.snapshots
}

func TestResumedStreamReplaysLongHistoryInBatches(t *testing.T) {
	hub := NewHub(5000, "journal")
	var events []flowevent.Event
	for i := 1; i <= 2500; i++ {
		events = append(events, flowevent.Event{Version: flowevent.Version, ID: flowevent.NewID("bulk", fmt.Sprint(i)), Time: time.Date(2026, 11, 25, 12, 0, 0, i, time.UTC),
			Component: "intake", Repository: "example/app", Issue: 7, Stage: "intake", Kind: flowevent.KindCompleted})
	}
	hub.Publish(events)
	server := httptest.NewServer((&Server{Hub: hub}).Handler())
	defer server.Close()
	stream, done := open(t, server.URL+"/events?since=1", "")
	defer done()
	got := readSSE(t, stream, 0, "ready")
	flows := 0
	for _, m := range got {
		if m.event == "flow" {
			flows++
		}
	}
	if flows != 2499 || got[len(got)-1].data != `{"cursor":2500}` {
		t.Fatal("resumed replay", flows, got[len(got)-1])
	}
}

func TestSnapshotCursorAndReadOnlySurface(t *testing.T) {
	hub := NewHub(10, "fixture")
	hub.Publish([]flowevent.Event{event(1), event(2)})
	server := httptest.NewServer((&Server{Hub: hub, Ready: func() bool { return true }}).Handler())
	defer server.Close()
	var snap Snapshot
	resp, err := http.Get(server.URL + "/api/snapshot?since=1")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&snap)
	resp.Body.Close()
	if snap.Version != SnapshotVersion || snap.Cursor != 2 || len(snap.Events) != 1 || snap.Events[0].Cursor != 2 || snap.Status.Source != "fixture" {
		t.Fatalf("snapshot %+v", snap)
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, _ := http.NewRequest(method, server.URL+"/api/snapshot", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatal(method, "accepted")
		}
		resp.Body.Close()
	}
	for path, want := range map[string]string{"/": "text/html", "/app.js": "javascript", "/app.css": "text/css"} {
		resp, err := http.Get(server.URL + path)
		if err != nil || resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), want) {
			t.Fatal(path, "not served", err)
		}
		if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal(path, "missing security headers")
		}
		resp.Body.Close()
	}
	if resp, _ := http.Get(server.URL + "/api/snapshot?since=x"); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("invalid cursor accepted")
	}
}

func TestEventStreamLimitsConcurrentClients(t *testing.T) {
	hub := NewHub(10, "journal")
	server := httptest.NewServer((&Server{Hub: hub, MaxStreams: 1}).Handler())
	defer server.Close()
	stream, done := open(t, server.URL+"/events", "")
	defer done()
	readSSE(t, stream, 0, "ready")
	resp, err := http.Get(server.URL + "/events")
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("second stream admitted")
	}
	resp.Body.Close()
}

func TestCollectorPublishesOnlyNewFactsAndReportsUnavailableJournal(t *testing.T) {
	hub := NewHub(100, "journal")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	journal := store.FlowJournal{ObservedAt: now, Intake: []store.FlowIntake{{Repository: "example/app", Issue: 7, Generation: 1, ObservedAt: now.Add(-time.Minute)}}}
	fail := false
	c := Collector{Hub: hub, Window: time.Hour, Limit: 10, Read: func(ctx context.Context, since time.Time, limit int) (store.FlowJournal, error) {
		if !since.Equal(now.Add(-time.Hour)) || limit != 10 {
			t.Fatal("collector window or limit not applied")
		}
		if fail {
			return store.FlowJournal{}, errors.New("unavailable")
		}
		return journal, nil
	}}
	if n, err := c.Poll(context.Background(), now); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	journal.Intake = append(journal.Intake, store.FlowIntake{Repository: "example/app", Issue: 7, Generation: 2, ObservedAt: now})
	if n, _ := c.Poll(context.Background(), now); n != 1 {
		t.Fatal("re-derived window not de-duplicated", n)
	}
	fail = true
	if _, err := c.Poll(context.Background(), now); err == nil || hub.Status().Healthy || hub.Status().Problem != "journal-unavailable" {
		t.Fatal("unavailable journal not reported", hub.Status())
	}
	if _, cursor, _ := hub.Since(0, 0); cursor != 2 {
		t.Fatal("failed poll changed history")
	}
}

// A row selected by a recent change can carry an event time older than the
// window (a work item created long ago that changed today). It must be
// published once, not forgotten and re-published on every poll.
func TestCollectorNeverRepublishesAnOldEventStillDerived(t *testing.T) {
	hub := NewHub(100, "journal")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * time.Hour)
	journal := store.FlowJournal{ObservedAt: now, WorkItems: []store.FlowWorkItem{{UUID: "8f9a2c1e-3b4d-4e5f-8a6b-7c8d9e0f1a2b", Repository: "example/app", Issues: []int{7}, Status: "ready", CreatedAt: old, UpdatedAt: now}}}
	c := Collector{Hub: hub, Window: 24 * time.Hour, Limit: 10, Read: func(context.Context, time.Time, int) (store.FlowJournal, error) { return journal, nil }}
	for poll := 0; poll < 5; poll++ {
		if n, err := c.Poll(context.Background(), now.Add(time.Duration(poll)*3*time.Second)); err != nil || (poll == 0) != (n == 1) {
			t.Fatalf("poll %d published %d (%v)", poll, n, err)
		}
	}
	if _, cursor, _ := hub.Since(0, 0); cursor != 1 {
		t.Fatal("old event re-published", cursor)
	}
	// Once no poll derives it any more, its id may be forgotten.
	journal.WorkItems = nil
	c.Poll(context.Background(), now)
	if len(hub.seen) != 0 {
		t.Fatal("de-duplication memory not bounded")
	}
}

func TestFixtureReleasesShiftedEventsLive(t *testing.T) {
	hub := NewHub(100, "fixture")
	events := []flowevent.Event{event(1), event(2), event(30)}
	start := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Fixture{Hub: hub, Events: events, Live: 20 * time.Second}.Run(ctx, start)
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, _, _ := hub.Since(0, 0)
		if len(entries) == 2 {
			// The recording is shifted to end 20 s after start: event(2) is
			// 28 s before the end, so it is history; event(30) is withheld.
			if !entries[1].Event.Time.Before(start) || entries[1].Event.ID != event(2).ID {
				t.Fatal("history not shifted", entries[1].Event.Time)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture history not released", len(entries))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
