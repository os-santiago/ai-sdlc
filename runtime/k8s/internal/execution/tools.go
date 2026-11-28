package execution

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/tektontasks"
)

// toolSession is the part of the executor session that carries generator tool
// calls: the fixed read-tool and respond-tool mailbox helpers.
type toolSession interface {
	ReadToolRequest(context.Context) (modelmailbox.Envelope, error)
	PublishToolResponse(context.Context, modelmailbox.Envelope) error
}

// ToolRunner runs one authorized-shape tool call, normally
// tektontasks.InvokeTool bound to the manifest, the generation's base revision,
// its budget and journal, and the sandbox runtime class.
type ToolRunner func(context.Context, tektontasks.ToolCall) (tektontasks.ToolResult, error)

// toolDispatch answers the generator's tool calls in order. A call is run at
// most once: the exact last request may be observed again until the mailbox
// consumes its answer, any other repeated or skipped sequence stops the run.
type toolDispatch struct {
	session  toolSession
	run      ToolRunner
	previous modelmailbox.Envelope
}

// poll answers one pending tool call, if any. The generator waits for the
// answer, so no model request is pending while a tool runs.
func (d *toolDispatch) poll(ctx context.Context) error {
	request, err := d.session.ReadToolRequest(ctx)
	if errors.Is(err, modelmailbox.ErrPending) {
		return nil
	}
	if err != nil {
		return driveStop{"execution-tool-read"}
	}
	if d.previous.Sequence != 0 && request.Sequence == d.previous.Sequence {
		if !sameRequest(request, d.previous) {
			return driveStop{"execution-tool-sequence"}
		}
		return nil
	}
	if request.Sequence != d.previous.Sequence+1 {
		return driveStop{"execution-tool-sequence"}
	}
	status, text := 200, ""
	call, err := tektontasks.ParseToolCall(request.Payload)
	if err == nil {
		var result tektontasks.ToolResult
		result, err = d.run(ctx, call)
		text = result.Text()
	}
	if err != nil {
		status, text = toolFailure(err)
	}
	response := modelmailbox.Envelope{Version: modelmailbox.Version, Kind: "tool-response", Attempt: request.Attempt, PodUID: request.PodUID,
		Sequence: request.Sequence, RequestSHA256: request.RequestSHA256, ExpiresAt: request.ExpiresAt, Status: status, Payload: toolPayload(text)}
	if ctx.Err() != nil {
		return driveStop{"execution-authority"}
	}
	if d.session.PublishToolResponse(ctx, response) != nil {
		return driveStop{"execution-tool-publish"}
	}
	d.previous = request
	d.previous.Payload = append([]byte(nil), request.Payload...)
	return nil
}

// toolFailure maps a refused or failed call to a fixed status and message.
// It never forwards an underlying error, cluster detail or log.
func toolFailure(err error) (int, string) {
	switch {
	case errors.Is(err, tektontasks.ErrToolBudget):
		return 429, "The tool budget of this generation is exhausted; finish without further tool calls."
	case errors.Is(err, tektontasks.ErrRequest):
		return 403, "The tool call is outside the reviewed manifest scope (tool, target, packages or candidate change)."
	case errors.Is(err, tektontasks.ErrDeadline), errors.Is(err, context.DeadlineExceeded):
		return 504, "The tool run did not finish within its bound."
	}
	return 500, "The tool run failed before producing a result."
}

// toolPayload encodes text within the mailbox tool response bound.
func toolPayload(text string) []byte {
	for limit := len(text); ; limit -= 1024 {
		if limit < 0 {
			limit = 0
		}
		raw, _ := json.Marshal(map[string]string{"text": text[:limit]})
		if len(raw) <= modelmailbox.MaxToolResponseBytes || limit == 0 {
			return raw
		}
	}
}
