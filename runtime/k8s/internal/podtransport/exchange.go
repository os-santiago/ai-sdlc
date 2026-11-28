package podtransport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
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
// accepts clean EOF without an explicit status. No fallback or retry exists
// here; only ReadRequest repeats a failed read exchange (see readExchange).
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
	if len(input) > operation.inputLimit || (!operation.stdin && len(input) != 0) {
		return nil, 0, ErrTransport
	}
	if !time.Now().Before(s.expires) {
		return nil, 0, transportError{"expired"}
	}
	ctx, cancel := context.WithDeadline(ctx, minDeadline(time.Now().Add(operation.timeout), s.expires))
	defer cancel()
	container := "mailbox"
	if s.helper != "" {
		container = s.helper
	}
	query := url.Values{"container": {container}, "stdout": {"true"}, "stderr": {"true"}, "tty": {"false"}}
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
		if errors.As(err, &failure) {
			var status apierrors.APIStatus
			if apierrors.IsForbidden(failure.Cause) {
				return nil, 0, ErrPermission
			}
			// A throttled or unavailable API server is transient; any other
			// refused upgrade (for example a vanished Pod) is not.
			if errors.As(failure.Cause, &status) && transientStatus(int(status.Status().Code)) {
				return nil, 0, s.interrupted(ctx)
			}
			return nil, 0, ErrTransport
		}
		// A certificate or TLS verification failure is a configuration fault,
		// not a transient one; the controller only ever dials https.
		if tlsRefusal(err) {
			return nil, 0, ErrTransport
		}
		return nil, 0, s.interrupted(ctx)
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
	// Each channel copies its messages into fixed chunks and joins them once
	// at the end: a growing buffer would briefly hold up to three times a
	// large export, and keeping each message would cost far more than its
	// bytes when messages are tiny.
	limits := map[byte]int{protocol.StreamStdOut: operation.outputLimit, protocol.StreamStdErr: 8192, protocol.StreamErr: 8192}
	stdout, stderr, status := newFrames(operation.outputLimit), newFrames(8192), newFrames(8192)
	buffers := map[byte]*frames{protocol.StreamStdOut: stdout, protocol.StreamStdErr: stderr, protocol.StreamErr: status}
	closed := false
	// operation.frames bounds messages that carry no bytes. Every other
	// message spends at least one byte of a channel limit, so the byte limits
	// already bound them, whatever chunk size the API server relays.
	for empty := 0; empty < operation.frames; {
		kind, reader, err := conn.NextReader()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				closed = true
				break
			}
			if errors.Is(err, websocket.ErrReadLimit) {
				return nil, 0, ErrTransport
			}
			return nil, 0, s.interrupted(ctx)
		}
		if kind != websocket.BinaryMessage {
			return nil, 0, ErrTransport
		}
		var channel [1]byte
		if _, err := io.ReadFull(reader, channel[:]); err != nil {
			return nil, 0, s.interrupted(ctx)
		}
		buffer, ok := buffers[channel[0]]
		if !ok {
			return nil, 0, ErrTransport
		}
		remaining := limits[channel[0]] - buffer.Len()
		raw, err := io.ReadAll(io.LimitReader(reader, int64(remaining)+1))
		if err != nil && !errors.Is(err, websocket.ErrReadLimit) {
			return nil, 0, s.interrupted(ctx)
		}
		if err != nil || len(raw) > remaining {
			return nil, 0, ErrTransport
		}
		if len(raw) == 0 {
			empty++
		}
		buffer.add(raw)
	}
	writeErr := <-written
	joined = true
	if ctx.Err() != nil || writeErr != nil {
		return nil, 0, s.interrupted(ctx)
	}
	if !closed {
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
	// From Kubernetes 1.30 the API server upgrades the client's WebSocket
	// before it dials the kubelet (its stream translator). A failed relay hop
	// arrives as this well-formed status after a 101, not as a refused upgrade.
	if result.Status == meta.StatusFailure && result.Reason == meta.StatusReasonInternalError && result.Code == http.StatusInternalServerError {
		return nil, 0, transportError{"relay"}
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
		return modelmailbox.Envelope{}, readFailure("before-observation", err)
	}
	raw, code, err := s.readExchange(ctx)
	if err != nil {
		return modelmailbox.Envelope{}, readFailure(transportStep(err), err)
	}
	// Validate the helper result before observing a concurrent worker exit.
	// An invalid envelope cannot be hidden behind a later zero exit.
	pending := code == 3 && len(raw) == 0
	var e modelmailbox.Envelope
	if !pending {
		if code != 0 {
			return e, readFailure("helper-exit-"+strconv.Itoa(code), ErrTransport)
		}
		e, err = modelmailbox.Decode(raw)
		if err != nil || e.Validate(s.identity, "request") != nil {
			return modelmailbox.Envelope{}, readFailure("envelope", ErrTransport)
		}
	}
	if err = s.requestWorkerActive(ctx); err != nil {
		// Preserve a validated observed request so the controller can reject
		// unhanded sequences/rebinding even when the writer has just exited.
		return e, readFailure("after-observation", err)
	}
	if pending {
		return modelmailbox.Envelope{}, modelmailbox.ErrPending
	}
	return e, nil
}

// readExchange runs the fixed read helper, repeating it after a transient
// transport failure (an exec error, a failed relay to the kubelet or the
// 5-second read deadline). Batteries
// 36, 37 and 39 lost whole attempts at model-mailbox-read-helper-transport and
// execution-read-helper-transport although the work was fine.
//
// Repeating is safe: the read helper only reads the pending request file and
// has no stdin, and the drive acknowledges a request (publishes its response)
// only after ReadRequest has returned, so no attempt reads after an
// acknowledgement. The respond exchange is never repeated.
//
// As with retryTransient, the window bounds only when another attempt may
// start, never an attempt: another starts while observeRetryWindow lasts or
// until minReadAttempts were made, and only while a whole attempt plus the
// after-observation still fit before the session expiry and ctx's deadline.
// A session expiry, a permission refusal, another refused upgrade (including
// a 5xx without a Status body), a certificate failure, a protocol violation
// or a cancelled ctx stops at once.
func (s *Session) readExchange(ctx context.Context) ([]byte, int, error) {
	last := time.Now().Add(observeRetryWindow)
	for attempts := 1; ; attempts++ {
		raw, code, err := s.exchange(ctx, "read", nil)
		var failed transportError
		if err == nil || !errors.As(err, &failed) || !failed.transient() {
			return raw, code, err
		}
		limit := s.expires
		if deadline, ok := ctx.Deadline(); ok {
			limit = minDeadline(limit, deadline)
		}
		more := time.Now().Before(last) || attempts < minReadAttempts
		if !more || time.Until(limit) < 2*readAttemptBound+250*time.Millisecond || !retryRemoval(ctx) {
			return raw, code, err
		}
	}
}

// transportError is a helper exchange interrupted by its cause: "exec" (the
// exec upgrade or stream failed, or the API server answered the upgrade with
// a 429/5xx Status), "relay" (the API server accepted the upgrade but its hop
// to the kubelet failed: an InternalError 500 status), "deadline" (the
// exchange's own deadline or the caller's ended first) or "expired" (the
// session's assignment expired). It unwraps to ErrTransport, so every caller
// decides exactly as before.
type transportError struct{ cause string }

func (e transportError) Error() string { return "helper exchange failed: " + e.cause }
func (e transportError) Unwrap() error { return ErrTransport }
func (e transportError) transient() bool {
	return e.cause == "exec" || e.cause == "relay" || e.cause == "deadline"
}

// tlsRefusal reports a certificate or TLS verification failure.
func tlsRefusal(err error) bool {
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	var verification *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	return errors.As(err, &unknown) || errors.As(err, &invalid) || errors.As(err, &hostname) || errors.As(err, &verification) || errors.As(err, &record)
}

// interrupted names why an exchange ended without a complete answer. A
// cancelled ctx is not a transport cause and keeps the plain ErrTransport.
// The connection's own deadline equals ctx's, so a read that timed out just
// before ctx's timer fired is a deadline too.
func (s *Session) interrupted(ctx context.Context) error {
	deadline, bounded := ctx.Deadline()
	switch {
	case !time.Now().Before(s.expires):
		return transportError{"expired"}
	case errors.Is(ctx.Err(), context.DeadlineExceeded), ctx.Err() == nil && bounded && !time.Now().Before(deadline):
		return transportError{"deadline"}
	case ctx.Err() != nil:
		return ErrTransport
	}
	return transportError{"exec"}
}

// transportStep is the read step of a failed read exchange: its cause when
// it has one, otherwise the plain helper-transport (a refused upgrade, a
// permission refusal, a protocol violation or a cancelled ctx).
func transportStep(err error) string {
	var failed transportError
	if errors.As(err, &failed) {
		return "helper-transport-" + failed.cause
	}
	return "helper-transport"
}

// readError names the bounded mailbox read step that failed. It unwraps to
// the original error (ErrWorkerExited, ErrTransport, ErrPermission), so every
// caller decides exactly as before; the step only refines the stop diagnostic.
type readError struct {
	step  string
	cause error
}

func (e readError) Error() string    { return "mailbox read failed at " + e.step }
func (e readError) Unwrap() error    { return e.cause }
func (e readError) ReadStep() string { return e.step }

func readFailure(step string, err error) error {
	if errors.Is(err, ErrWorkerExited) {
		// An observed exit is not a read failure; callers handle it.
		return err
	}
	return readError{step: step, cause: err}
}

var readSteps = regexp.MustCompile(`^(before-observation|helper-transport(-(exec|relay|deadline|expired))?|envelope|after-observation|helper-exit-([1-9][0-9]?|1[0-9][0-9]|2[0-4][0-9]|25[0-5]))$`)

// ReadStep returns the failed mailbox read step, or "" for any other error.
func ReadStep(err error) string {
	var failed readError
	if errors.As(err, &failed) && readSteps.MatchString(failed.step) {
		return failed.step
	}
	return ""
}

// KnownReadStep reports whether step is one ReadStep can return.
func KnownReadStep(step string) bool { return readSteps.MatchString(step) }

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
	// One observation that also reports a terminated worker: publication is
	// refused exactly as before, and an exit of the pinned worker instance
	// (for example 124 when the launcher's run budget ended while this call
	// was in flight) is named by its bounded exit code. A retry of this read
	// must leave the respond exchange and the after-observation their time, or
	// a clean refusal before anything was sent would become an ambiguous one.
	observed, err := s.inspect(withReadReserve(ctx, 2*readAttemptBound), true)
	if err != nil {
		return err
	}
	if observed.workerExitCode != nil {
		if observed.workerTerminalID != observed.instances["worker"].ID {
			return ErrTransport
		}
		return workerExitedError{code: *observed.workerExitCode, cause: ErrTransport}
	}
	stage = "helper-transport"
	receipt, code, err := s.exchange(ctx, "respond", raw)
	if err != nil {
		return err
	}
	stage = "helper-exit"
	if code != 0 {
		// The mailbox helper's bounded exit code (1-255) names the cause class:
		// 124 its 5 s deadline or a termination signal, 3 a vanished staging
		// file, 1 any other refusal (see executor-failure-diagnostics.md).
		if code > 0 && code <= 255 {
			stage = "helper-exit-" + strconv.Itoa(code)
		}
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

// workerExitedError is a refused publication because the pinned worker
// instance has already exited. It unwraps to ErrTransport.
type workerExitedError struct {
	code  int32
	cause error
}

func (e workerExitedError) Error() string { return "worker exited before publication" }
func (e workerExitedError) WorkerExitCode() (int32, bool) {
	return e.code, e.code >= 0 && e.code <= 255
}
func (e workerExitedError) Unwrap() error { return e.cause }

// Only fixed publication boundaries are surfaced; never helper stderr or API
// exception text. Unwrap preserves the existing error classification.
type publicationError struct {
	stage string
	cause error
}

func (e publicationError) Error() string            { return "mailbox publication failed at " + e.stage }
func (e publicationError) Unwrap() error            { return e.cause }
func (e publicationError) PublicationStage() string { return e.stage }

// frames accumulates one channel's messages in fixed-size chunks: memory
// stays close to the bytes received however small the messages are (a
// helper flushing one byte per message would otherwise make each message
// cost hundreds of bytes), and no buffer is regrown.
type frames struct {
	chunk int
	parts [][]byte
	size  int
}

func newFrames(limit int) *frames {
	return &frames{chunk: min(max(limit, 1), 1<<20)}
}

func (f *frames) add(p []byte) {
	for len(p) > 0 {
		if len(f.parts) == 0 || len(f.parts[len(f.parts)-1]) == cap(f.parts[len(f.parts)-1]) {
			f.parts = append(f.parts, make([]byte, 0, f.chunk))
		}
		last := &f.parts[len(f.parts)-1]
		n := min(len(p), cap(*last)-len(*last))
		*last = append(*last, p[:n]...)
		p = p[n:]
		f.size += n
	}
}
func (f *frames) Len() int { return f.size }

// Bytes joins the chunks into one exact allocation and releases them.
func (f *frames) Bytes() []byte {
	if f.size == 0 {
		return nil
	}
	joined := make([]byte, 0, f.size)
	for _, p := range f.parts {
		joined = append(joined, p...)
	}
	f.parts = nil
	return joined
}
