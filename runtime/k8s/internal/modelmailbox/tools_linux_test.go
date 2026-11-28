package modelmailbox

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func setupToolServer(t *testing.T, limits ToolLimits) (*Store, *httptest.Server) {
	t.Helper()
	s, _ := newTestStore(t)
	handler, err := NewServer(s, testIdentity, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.EnableTools(limits); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return s, httpServer
}

func sendTool(httpServer *httptest.Server, body []byte) <-chan httpResult {
	done := make(chan httpResult, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		r, err := client.Post(httpServer.URL+"/v1/ai-sdlc/tools", "application/json", bytes.NewReader(body))
		if err != nil {
			done <- httpResult{err: err}
			return
		}
		defer r.Body.Close()
		raw, err := io.ReadAll(r.Body)
		done <- httpResult{status: r.StatusCode, body: raw, err: err}
	}()
	return done
}

func awaitToolRequest(t *testing.T, s *Store) Envelope {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e, err := s.ReadToolRequest(testIdentity); err == nil {
			return e
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("tool request not published")
	return Envelope{}
}

const toolCall = `{"tool":"build_test","target":"rest-villains","diff":"--- a/A\n+++ b/A\n"}`

func TestToolCallsRelayThroughTheMailboxAndBindTheirAnswer(t *testing.T) {
	s, httpServer := setupToolServer(t, ToolLimits{MaxCalls: 1, Wait: 2 * time.Second})
	done := sendTool(httpServer, []byte(toolCall))
	request := awaitToolRequest(t, s)
	if string(request.Payload) != toolCall || request.Kind != "tool-request" || request.Sequence != 1 {
		t.Fatal("published tool request", request)
	}
	// An answer for another request is refused by the store.
	wrong := Envelope{Version: Version, Kind: "tool-response", Attempt: testIdentity.Attempt, PodUID: testIdentity.PodUID,
		Sequence: 1, RequestSHA256: Digest([]byte("other")), Status: 200, ExpiresAt: request.ExpiresAt, Payload: []byte(`{}`)}
	if s.WriteToolResponse(testIdentity, wrong) == nil {
		t.Fatal("accepted a response to another request")
	}
	answer := request
	answer.Kind, answer.Status, answer.Payload = "tool-response", 200, []byte(`{"text":"Untrusted tool output"}`)
	if err := s.WriteToolResponse(testIdentity, answer); err != nil {
		t.Fatal(err)
	}
	r := result(t, done, 200)
	if string(r.body) != `{"text":"Untrusted tool output"}` {
		t.Fatal("tool answer", string(r.body))
	}
	// The local call limit holds after the first call.
	result(t, sendTool(httpServer, []byte(toolCall)), 429)
}

func TestToolRouteBoundsRequestsAndTimesOut(t *testing.T) {
	s, httpServer := setupToolServer(t, ToolLimits{MaxCalls: 3, Wait: time.Second})
	for _, bad := range []string{`{"tool":"build_test"}`, `{"tool":"build_test","target":"m","command":"sh"}`, `[]`} {
		result(t, sendTool(httpServer, []byte(bad)), 400)
	}
	big := `{"tool":"build_test","target":"m","diff":"` + strings.Repeat("x", MaxToolRequestBytes) + `"}`
	result(t, sendTool(httpServer, []byte(big)), 413)
	done := sendTool(httpServer, []byte(toolCall))
	expired := awaitToolRequest(t, s)
	result(t, done, 504)
	if _, err := s.ReadToolRequest(testIdentity); err == nil {
		t.Fatal("an expired tool request was left behind")
	}
	// A late answer finds nothing pending (the helper's exit 3), which the
	// controller treats as a call that already ended for the generator.
	late := expired
	late.Kind, late.Status, late.Payload, late.ExpiresAt = "tool-response", 200, []byte(`{"text":"late"}`), time.Now().Add(time.Minute).UnixMilli()
	if err := s.WriteToolResponse(testIdentity, late); !errors.Is(err, ErrPending) {
		t.Fatal("late answer", err)
	}
}

// One call at a time: a second call while one is pending is refused at once
// with tool_call_already_pending, and the pending call is unaffected.
func TestToolRouteRefusesASecondConcurrentCall(t *testing.T) {
	s, httpServer := setupToolServer(t, ToolLimits{MaxCalls: 3, Wait: 2 * time.Second})
	first := sendTool(httpServer, []byte(toolCall))
	request := awaitToolRequest(t, s)
	second := result(t, sendTool(httpServer, []byte(toolCall)), 429)
	if !strings.Contains(string(second.body), "tool_call_already_pending") {
		t.Fatal("second call", string(second.body))
	}
	answer := request
	answer.Kind, answer.Status, answer.Payload = "tool-response", 200, []byte(`{"text":"ok"}`)
	if err := s.WriteToolResponse(testIdentity, answer); err != nil {
		t.Fatal(err)
	}
	result(t, first, 200)
}

func TestToolRouteIsOffUnlessEnabled(t *testing.T) {
	_, _, httpServer := setupServer(t, time.Second)
	result(t, sendTool(httpServer, []byte(toolCall)), 404)
	s, _ := newTestStore(t)
	server, _ := NewServer(s, testIdentity, time.Second)
	for _, limits := range []ToolLimits{{MaxCalls: 0, Wait: time.Second}, {MaxCalls: MaxToolCalls + 1, Wait: time.Second}, {MaxCalls: 1, Wait: time.Hour}} {
		if server.EnableTools(limits) == nil {
			t.Fatal("accepted tool limits", limits)
		}
	}
}
