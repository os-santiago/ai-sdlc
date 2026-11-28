package execution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/tektontasks"
)

type fakeToolSession struct {
	requests  []modelmailbox.Envelope
	published []modelmailbox.Envelope
	failRead  bool
}

func (f *fakeToolSession) ReadToolRequest(context.Context) (modelmailbox.Envelope, error) {
	if f.failRead {
		return modelmailbox.Envelope{}, errors.New("transport")
	}
	if len(f.requests) == 0 {
		return modelmailbox.Envelope{}, modelmailbox.ErrPending
	}
	r := f.requests[0]
	f.requests = f.requests[1:]
	return r, nil
}

func (f *fakeToolSession) PublishToolResponse(_ context.Context, e modelmailbox.Envelope) error {
	f.published = append(f.published, e)
	return nil
}

func toolRequest(sequence uint64, payload string) modelmailbox.Envelope {
	return modelmailbox.Envelope{Version: modelmailbox.Version, Kind: "tool-request", Attempt: "11111111-1111-4111-8111-111111111111-1",
		PodUID: "22222222-2222-4222-8222-222222222222", Sequence: sequence, RequestSHA256: modelmailbox.Digest([]byte(payload)),
		ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), Payload: []byte(payload)}
}

// Each tool call runs once and is answered with its own binding; a repeated
// observation of the answered call is not run again; refusals map to fixed
// statuses without forwarding the cause; a skipped or rewritten sequence and
// a failed read stop the run.
func TestToolDispatchRunsEachCallOnceInOrder(t *testing.T) {
	first := toolRequest(1, `{"tool":"build_test","target":"rest-villains","diff":""}`)
	session := &fakeToolSession{requests: []modelmailbox.Envelope{first, first, toolRequest(2, `{"tool":"build_test","target":"x","diff":""}`)}}
	runs := 0
	d := &toolDispatch{session: session, run: func(_ context.Context, call tektontasks.ToolCall) (tektontasks.ToolResult, error) {
		runs++
		if call.Target == "x" {
			return tektontasks.ToolResult{}, errors.New("secret cluster detail")
		}
		return tektontasks.ToolResult{Tool: call.Tool, Target: call.Target, Passed: true, Summary: json.RawMessage(`{"tests":94}`)}, nil
	}}
	for range 4 {
		if err := d.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if runs != 2 || len(session.published) != 2 {
		t.Fatal("runs", runs, len(session.published))
	}
	ok, failed := session.published[0], session.published[1]
	if ok.Status != 200 || ok.Sequence != 1 || ok.RequestSHA256 != first.RequestSHA256 || ok.Kind != "tool-response" || !strings.Contains(string(ok.Payload), "Untrusted tool output") {
		t.Fatal("answer", ok.Status, string(ok.Payload))
	}
	if failed.Status != 500 || strings.Contains(string(failed.Payload), "secret") {
		t.Fatal("failure leaked or misclassified", failed.Status, string(failed.Payload))
	}
	for err, status := range map[error]int{tektontasks.ErrToolBudget: 429, tektontasks.ErrRequest: 403, tektontasks.ErrDeadline: 504, context.DeadlineExceeded: 504} {
		if got, _ := toolFailure(err); got != status {
			t.Fatal("status", err, got)
		}
	}
	for _, s := range []*fakeToolSession{
		{requests: []modelmailbox.Envelope{toolRequest(2, `{"tool":"a","target":"b","diff":""}`)}},
		{failRead: true},
	} {
		if (&toolDispatch{session: s, run: func(context.Context, tektontasks.ToolCall) (tektontasks.ToolResult, error) {
			t.Fatal("ran")
			return tektontasks.ToolResult{}, nil
		}}).poll(context.Background()) == nil {
			t.Fatal("sequence gap or read failure accepted")
		}
	}
	rewritten := &toolDispatch{session: &fakeToolSession{requests: []modelmailbox.Envelope{toolRequest(1, `{"tool":"a","target":"c","diff":""}`)}}, previous: first}
	if rewritten.poll(context.Background()) == nil {
		t.Fatal("rewritten sequence accepted")
	}
	if len(toolPayload(strings.Repeat("x", 64*1024))) > modelmailbox.MaxToolResponseBytes {
		t.Fatal("payload bound")
	}
}
