package podtransport

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
)

// Generator tool calls use two more fixed helper modes of the same mailbox
// container: read-tool returns the pending tool request (exit 3 when none is
// pending) and respond-tool publishes the controller's answer to exactly that
// request. Both are bound to the session's attempt and Pod UID like read and
// respond; neither accepts a path, a command or a size from the caller.
const (
	toolReadOutputLimit = modelmailbox.MaxToolEnvelopeBytes + 1024
	toolReceiptLimit    = 256
)

// ReadToolRequest returns the pending generator tool request, validated, or
// modelmailbox.ErrPending when none is pending.
func (s *Session) ReadToolRequest(ctx context.Context) (modelmailbox.Envelope, error) {
	if err := s.requestWorkerActive(ctx); err != nil {
		return modelmailbox.Envelope{}, readFailure("before-observation", err)
	}
	raw, code, err := s.exchangeFixed(ctx, helperOperation{
		command:     []string{"/model-mailbox", "read-tool", "--attempt", s.identity.Attempt, "--pod-uid", s.identity.PodUID},
		outputLimit: toolReadOutputLimit, frames: 4096, timeout: 15 * time.Second,
	}, nil)
	if err != nil {
		return modelmailbox.Envelope{}, readFailure(transportStep(err), err)
	}
	if code == 3 && len(raw) == 0 {
		return modelmailbox.Envelope{}, modelmailbox.ErrPending
	}
	if code != 0 {
		return modelmailbox.Envelope{}, readFailure("helper-exit-"+strconv.Itoa(code), ErrTransport)
	}
	e, err := modelmailbox.DecodeWithin(raw, toolReadOutputLimit)
	if err != nil || e.Validate(s.identity, "tool-request") != nil {
		return modelmailbox.Envelope{}, readFailure("envelope", ErrTransport)
	}
	return e, nil
}

// ErrToolNotPending is a tool answer the mailbox no longer waits for: the
// call's window passed (the generator received 504) or its caller left.
var ErrToolNotPending = errors.New("tool request no longer pending")

// PublishToolResponse answers the pending tool request. The helper refuses a
// response that does not match the pending request's sequence, digest and
// expiry; success is confirmed by its receipt naming the request digest.
func (s *Session) PublishToolResponse(ctx context.Context, e modelmailbox.Envelope) error {
	if e.Validate(s.identity, "tool-response") != nil {
		return ErrTransport
	}
	raw, err := json.Marshal(e)
	if err != nil || len(raw) > modelmailbox.MaxEnvelopeBytes {
		return ErrTransport
	}
	receipt, code, err := s.exchangeFixed(ctx, helperOperation{
		command:    []string{"/model-mailbox", "respond-tool", "--attempt", s.identity.Attempt, "--pod-uid", s.identity.PodUID},
		inputLimit: modelmailbox.MaxEnvelopeBytes, outputLimit: toolReceiptLimit, frames: 64, timeout: 5 * time.Second, stdin: true,
	}, raw)
	if err != nil {
		return publicationError{stage: "helper-transport", cause: err}
	}
	if code == 3 {
		return ErrToolNotPending
	}
	if code != 0 {
		return publicationError{stage: "helper-exit-" + strconv.Itoa(code), cause: ErrTransport}
	}
	var published struct {
		Published string `json:"published"`
	}
	if json.Unmarshal(receipt, &published) != nil || published.Published != e.RequestSHA256 {
		return publicationError{stage: "receipt", cause: ErrTransport}
	}
	return nil
}
