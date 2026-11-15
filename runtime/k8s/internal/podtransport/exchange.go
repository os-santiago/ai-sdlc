package podtransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	protocol "k8s.io/apimachinery/pkg/util/remotecommand"
	upgrade "k8s.io/client-go/transport/websocket"
)

// exchange implements only the V5 channels needed by two fixed helper modes.
// client-go owns verified upgrade/authentication; its generic remotecommand
// reader is intentionally not used because its status channel is unbounded and
// accepts clean EOF without an explicit status. No fallback or retry exists.
func (s *Session) exchange(ctx context.Context, mode string, input []byte) ([]byte, int, error) {
	limit := modelmailbox.MaxEnvelopeBytes
	if mode == "respond" {
		limit = modelmailbox.MaxReceiptBytes
	} else if mode != "read" {
		return nil, 0, ErrTransport
	}
	return s.exchangeFixed(ctx, helperOperation{
		command:    []string{"/model-mailbox", mode, "--attempt", s.identity.Attempt, "--pod-uid", s.identity.PodUID},
		inputLimit: modelmailbox.MaxEnvelopeBytes, outputLimit: limit, frames: 1024, timeout: 5 * time.Second, stdin: mode == "respond",
	}, input)
}

type helperOperation struct {
	command                         []string
	inputLimit, outputLimit, frames int
	timeout                         time.Duration
	stdin                           bool
}

// Private callers construct fixed helpers. No exported API accepts commands,
// containers, destinations, byte limits or transport settings.
func (s *Session) exchangeFixed(ctx context.Context, operation helperOperation, input []byte) ([]byte, int, error) {
	if !time.Now().Before(s.expires) || len(input) > operation.inputLimit || (!operation.stdin && len(input) != 0) {
		return nil, 0, ErrTransport
	}
	ctx, cancel := context.WithDeadline(ctx, minDeadline(time.Now().Add(operation.timeout), s.expires))
	defer cancel()
	query := url.Values{"container": {"mailbox"}, "stdout": {"true"}, "stderr": {"true"}, "tty": {"false"}}
	query["command"] = operation.command
	if operation.stdin {
		query.Set("stdin", "true")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.podURL()+"/exec?"+query.Encode(), nil)
	if err != nil {
		return nil, 0, ErrTransport
	}
	roundTripper, holder, err := upgrade.RoundTripperFor(s.client.config)
	if err != nil {
		return nil, 0, ErrTransport
	}
	conn, err := upgrade.Negotiate(roundTripper, holder, request, protocol.StreamProtocolV5Name)
	if err != nil {
		var failure *httpstream.UpgradeFailureError
		if errors.As(err, &failure) && apierrors.IsForbidden(failure.Cause) {
			return nil, 0, ErrPermission
		}
		return nil, 0, ErrTransport
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if conn.SetReadDeadline(deadline) != nil || conn.SetWriteDeadline(deadline) != nil {
		return nil, 0, ErrTransport
	}
	conn.SetReadLimit(int64(max(operation.outputLimit, 8192) + 1))
	written := make(chan error, 1)
	go func() {
		if operation.stdin {
			for remaining := input; len(remaining) > 0; {
				n := min(len(remaining), 16*1024)
				frame := make([]byte, n+1)
				frame[0] = protocol.StreamStdIn
				copy(frame[1:], remaining[:n])
				if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
					written <- err
					return
				}
				remaining = remaining[n:]
			}
			if err := conn.WriteMessage(websocket.BinaryMessage, []byte{protocol.StreamClose, protocol.StreamStdIn}); err != nil {
				written <- err
				return
			}
		}
		written <- nil
	}()
	// Closing the connection unblocks both directions. Join the sole writer on
	// every exit; no goroutine retains credentials or response bytes afterward.
	joined := false
	defer func() {
		conn.Close()
		if !joined {
			<-written
		}
	}()
	var stdout, stderr, status bytes.Buffer
	limits := map[byte]int{protocol.StreamStdOut: operation.outputLimit, protocol.StreamStdErr: 8192, protocol.StreamErr: 8192}
	buffers := map[byte]*bytes.Buffer{protocol.StreamStdOut: &stdout, protocol.StreamStdErr: &stderr, protocol.StreamErr: &status}
	closed := false
	for frames := 0; frames < operation.frames; frames++ {
		kind, reader, err := conn.NextReader()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				closed = true
				break
			}
			return nil, 0, ErrTransport
		}
		if kind != websocket.BinaryMessage {
			return nil, 0, ErrTransport
		}
		var channel [1]byte
		if _, err := io.ReadFull(reader, channel[:]); err != nil {
			return nil, 0, ErrTransport
		}
		buffer, ok := buffers[channel[0]]
		if !ok {
			return nil, 0, ErrTransport
		}
		remaining := limits[channel[0]] - buffer.Len()
		raw, err := io.ReadAll(io.LimitReader(reader, int64(remaining)+1))
		if err != nil || len(raw) > remaining {
			return nil, 0, ErrTransport
		}
		buffer.Write(raw)
	}
	writeErr := <-written
	joined = true
	if !closed || ctx.Err() != nil || writeErr != nil {
		return nil, 0, ErrTransport
	}
	var result meta.Status
	if status.Len() == 0 || authority.DecodeStrict(status.Bytes(), &result) != nil {
		return nil, 0, ErrTransport
	}
	if result.Status == meta.StatusSuccess && result.Reason == "" && result.Details == nil && (result.Code == 0 || result.Code == 200) {
		if stderr.Len() != 0 {
			return nil, 0, ErrTransport
		}
		return stdout.Bytes(), 0, nil
	}
	if result.Status == meta.StatusFailure && result.Reason == protocol.NonZeroExitCodeReason && result.Details != nil && len(result.Details.Causes) == 1 && result.Details.Causes[0].Type == protocol.ExitCodeCauseType {
		code, err := strconv.ParseUint(result.Details.Causes[0].Message, 10, 8)
		if err == nil && code > 0 {
			return stdout.Bytes(), int(code), nil
		}
	}
	return nil, 0, ErrTransport
}

func (s *Session) requestWorkerActive(ctx context.Context) error {
	if s == nil {
		return ErrTransport
	}
	observed, err := s.inspect(ctx, true)
	if err != nil {
		return err
	}
	if observed.workerExitCode != nil {
		return ErrWorkerExited
	}
	return nil
}

func (s *Session) ReadRequest(ctx context.Context) (modelmailbox.Envelope, error) {
	if err := s.requestWorkerActive(ctx); err != nil {
		return modelmailbox.Envelope{}, err
	}
	raw, code, err := s.exchange(ctx, "read", nil)
	if err != nil {
		return modelmailbox.Envelope{}, err
	}
	// Validate the helper result before observing a concurrent worker exit.
	// An invalid envelope cannot be hidden behind a later zero exit.
	pending := code == 3 && len(raw) == 0
	var e modelmailbox.Envelope
	if !pending {
		if code != 0 {
			return e, ErrTransport
		}
		e, err = modelmailbox.Decode(raw)
		if err != nil || e.Validate(s.identity, "request") != nil {
			return modelmailbox.Envelope{}, ErrTransport
		}
	}
	if err = s.requestWorkerActive(ctx); err != nil {
		// Preserve a validated observed request so the controller can reject
		// unhanded sequences/rebinding even when the writer has just exited.
		return e, err
	}
	if pending {
		return modelmailbox.Envelope{}, modelmailbox.ErrPending
	}
	return e, nil
}

// Publish implements modelbroker.Publisher. Success confirms exclusive helper
// publication through its exact receipt; it never claims application receipt.
func (s *Session) Publish(ctx context.Context, e modelmailbox.Envelope) (err error) {
	stage := "envelope"
	defer func() {
		if err != nil {
			err = publicationError{stage: stage, cause: err}
		}
	}()
	if e.Validate(s.identity, "response") != nil {
		return ErrTransport
	}
	e.Payload = append([]byte(nil), e.Payload...)
	stage = "encoding"
	raw, err := json.Marshal(e)
	if err != nil || len(raw) > modelmailbox.MaxEnvelopeBytes {
		return ErrTransport
	}
	stage = "before-observation"
	if _, err := s.observe(ctx, false); err != nil {
		return err
	}
	stage = "helper-transport"
	receipt, code, err := s.exchange(ctx, "respond", raw)
	if err != nil {
		return err
	}
	stage = "helper-exit"
	if code != 0 {
		return ErrTransport
	}
	stage = "receipt"
	if modelmailbox.ValidatePublicationReceipt(receipt, e) != nil {
		return ErrTransport
	}
	stage = "after-observation"
	if _, err := s.observe(ctx, true); err != nil {
		return err
	}
	return nil
}

// Only fixed publication boundaries are surfaced; never helper stderr or API
// exception text. Unwrap preserves the existing error classification.
type publicationError struct {
	stage string
	cause error
}

func (e publicationError) Error() string            { return "mailbox publication failed at " + e.stage }
func (e publicationError) Unwrap() error            { return e.cause }
func (e publicationError) PublicationStage() string { return e.stage }
