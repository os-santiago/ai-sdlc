package podtransport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	protocol "k8s.io/apimachinery/pkg/util/remotecommand"
)

// readExecFixture serves the session's pod and answers each read exec with
// behave(n), n counting exec upgrades from 1.
func readExecFixture(t *testing.T, behave func(n int32, w http.ResponseWriter, r *http.Request)) (*Session, *atomic.Int32) {
	t.Helper()
	binding, pod, _ := bindingFixture(t)
	var execs atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/exec") {
			json.NewEncoder(w).Encode(pod)
			return
		}
		if q := r.URL.Query(); q.Get("stdin") != "" || len(q["command"]) < 2 || q["command"][1] != "read" {
			t.Error("only the read helper may be repeated")
			w.WriteHeader(400)
			return
		}
		behave(execs.Add(1), w, r)
	}))
	t.Cleanup(server.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	client, err := NewClient(Config{APIServer: server.URL, CA: ca, BearerToken: strings.Repeat("x", 32)})
	if err != nil {
		t.Fatal(err)
	}
	session, err := Bind(context.Background(), client, binding)
	if err != nil {
		t.Fatal(err)
	}
	return session, &execs
}

func upgradeExec(t *testing.T, w http.ResponseWriter, r *http.Request) *websocket.Conn {
	u := websocket.Upgrader{Subprotocols: []string{protocol.StreamProtocolV5Name}}
	conn, err := u.Upgrade(w, r, nil)
	if err != nil {
		t.Error(err)
		return nil
	}
	return conn
}

// answerPending is a complete read whose helper reports no pending request.
func answerPending(t *testing.T, w http.ResponseWriter, r *http.Request) {
	conn := upgradeExec(t, w, r)
	if conn == nil {
		return
	}
	defer conn.Close()
	status := `{"status":"Failure","reason":"NonZeroExitCode","details":{"causes":[{"reason":"ExitCode","message":"3"}]}}`
	_ = conn.WriteMessage(websocket.BinaryMessage, append([]byte{protocol.StreamErr}, status...))
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
}

// dropExec upgrades and then drops the stream without a status or closure.
func dropExec(t *testing.T, w http.ResponseWriter, r *http.Request) {
	if conn := upgradeExec(t, w, r); conn != nil {
		conn.NetConn().Close()
	}
}

func unavailableExec(w http.ResponseWriter, code int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"` + reason + `","code":` + strconv.Itoa(code) + `}`))
}

// plainExec answers the upgrade with a 5xx whose body is not a Status.
func plainExec(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(code)
	w.Write([]byte("upstream unavailable"))
}

// relayFailure is the API server's stream translator (Kubernetes 1.30 and
// later) after its dial to the kubelet failed: a 101, then an InternalError
// status on the error channel and a normal closure.
func relayFailure(t *testing.T, w http.ResponseWriter, r *http.Request) {
	conn := upgradeExec(t, w, r)
	if conn == nil {
		return
	}
	defer conn.Close()
	status := `{"metadata":{},"status":"Failure","message":"Internal error occurred: error dialing backend","reason":"InternalError","details":{"causes":[{"message":"error dialing backend"}]},"code":500}`
	_ = conn.WriteMessage(websocket.BinaryMessage, append([]byte{protocol.StreamErr}, status...))
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
}

func TestMailboxReadRetriesTransientTransportFailures(t *testing.T) {
	for _, kind := range []string{"dropped", "unavailable", "internal", "bad-gateway", "gateway-timeout", "throttled", "relay", "slow"} {
		t.Run(kind, func(t *testing.T) {
			session, execs := readExecFixture(t, func(n int32, w http.ResponseWriter, r *http.Request) {
				if n > 1 {
					answerPending(t, w, r)
					return
				}
				switch kind {
				case "dropped":
					dropExec(t, w, r)
				case "unavailable":
					unavailableExec(w, 503, "ServiceUnavailable")
				case "internal":
					unavailableExec(w, 500, "InternalError")
				case "bad-gateway":
					unavailableExec(w, 502, "InternalError")
				case "gateway-timeout":
					unavailableExec(w, 504, "Timeout")
				case "throttled":
					unavailableExec(w, 429, "TooManyRequests")
				case "relay":
					relayFailure(t, w, r)
				case "slow":
					// The first exchange outlives its own 5-second read deadline.
					if conn := upgradeExec(t, w, r); conn != nil {
						time.Sleep(readAttemptBound + 500*time.Millisecond)
						conn.Close()
					}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			if _, err := session.ReadRequest(ctx); !errors.Is(err, modelmailbox.ErrPending) {
				t.Fatalf("transient %s failure was not recovered: %v (step %q)", kind, err, ReadStep(err))
			}
			if execs.Load() != 2 {
				t.Fatalf("%d read exchanges, want 2", execs.Load())
			}
		})
	}
}

func TestMailboxReadNamesTheTransportCauseAndStopsWithinItsBounds(t *testing.T) {
	type outcome struct {
		step    string
		cause   error
		execs   func(int32) bool
		within  time.Duration
		timeout time.Duration
	}
	once := func(n int32) bool { return n == 1 }
	cases := map[string]outcome{
		// A persistently dropped stream is retried only while attempts may start.
		// Each quick failure is followed by a 250 ms pause, so at most about
		// window/pause+1 attempts start.
		"persistent-drop": {"helper-transport-exec", ErrTransport, func(n int32) bool {
			return n >= minReadAttempts && n <= int32(observeRetryWindow/(250*time.Millisecond))+2
		}, observeRetryWindow + 3*time.Second, 40 * time.Second},
		"persistent-relay": {"helper-transport-relay", ErrTransport, func(n int32) bool {
			return n >= minReadAttempts && n <= int32(observeRetryWindow/(250*time.Millisecond))+2
		}, observeRetryWindow + 3*time.Second, 40 * time.Second},
		// A cancelled controller is not a transport cause and is not retried.
		"cancelled": {"helper-transport", ErrTransport, once, 3 * time.Second, 40 * time.Second},
		// A 5xx without a Status body cannot be told from a misrouted answer.
		"plain-5xx": {"helper-transport", ErrTransport, once, time.Second, 40 * time.Second},
		// No retry starts when a whole attempt and the after-observation no
		// longer fit before the caller's deadline.
		"drop-near-deadline": {"helper-transport-exec", ErrTransport, once, time.Second, 9 * time.Second},
		// The caller's deadline ends a stalled exchange; it is named, not retried.
		"stall-past-deadline": {"helper-transport-deadline", ErrTransport, once, 3 * time.Second, 1500 * time.Millisecond},
		// The session expires during a stalled exchange.
		"expired": {"helper-transport-expired", ErrTransport, once, 3 * time.Second, 40 * time.Second},
		// Non-transient failures keep the plain step and stop at once.
		"forbidden":     {"helper-transport", ErrPermission, once, time.Second, 40 * time.Second},
		"refused":       {"helper-transport", ErrTransport, once, time.Second, 40 * time.Second},
		"protocol":      {"helper-transport", ErrTransport, once, time.Second, 40 * time.Second},
		"helper-exit-1": {"helper-exit-1", ErrTransport, once, time.Second, 40 * time.Second},
	}
	for kind, want := range cases {
		t.Run(kind, func(t *testing.T) {
			session, execs := readExecFixture(t, func(n int32, w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "persistent-drop", "drop-near-deadline":
					dropExec(t, w, r)
				case "persistent-relay":
					relayFailure(t, w, r)
				case "plain-5xx":
					plainExec(w, 503)
				case "stall-past-deadline", "expired", "cancelled":
					if conn := upgradeExec(t, w, r); conn != nil {
						time.Sleep(2 * time.Second)
						conn.Close()
					}
				case "forbidden":
					unavailableExec(w, 403, "Forbidden")
				case "refused":
					unavailableExec(w, 404, "NotFound")
				case "protocol":
					if conn := upgradeExec(t, w, r); conn != nil {
						_ = conn.WriteMessage(websocket.TextMessage, []byte("unexpected"))
						conn.Close()
					}
				case "helper-exit-1":
					if conn := upgradeExec(t, w, r); conn != nil {
						status := `{"status":"Failure","reason":"NonZeroExitCode","details":{"causes":[{"reason":"ExitCode","message":"1"}]}}`
						_ = conn.WriteMessage(websocket.BinaryMessage, append([]byte{protocol.StreamErr}, status...))
						_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
						conn.Close()
					}
				}
			})
			if kind == "expired" {
				// Leave the observation before the read its time; expire mid-read.
				session.expires = time.Now().Add(700 * time.Millisecond)
			}
			ctx, cancel := context.WithTimeout(context.Background(), want.timeout)
			defer cancel()
			if kind == "cancelled" {
				stop := time.AfterFunc(500*time.Millisecond, cancel)
				defer stop.Stop()
			}
			start := time.Now()
			_, err := session.ReadRequest(ctx)
			if ReadStep(err) != want.step || !KnownReadStep(want.step) || !errors.Is(err, want.cause) {
				t.Fatalf("got %v (step %q), want step %q", err, ReadStep(err), want.step)
			}
			if !want.execs(execs.Load()) {
				t.Fatalf("%d read exchanges", execs.Load())
			}
			if elapsed := time.Since(start); elapsed > want.within {
				t.Fatalf("stopped after %v, want within %v", elapsed, want.within)
			}
		})
	}
}

func TestReadTransportStepsAreBounded(t *testing.T) {
	for _, step := range []string{"helper-transport", "helper-transport-exec", "helper-transport-relay", "helper-transport-deadline", "helper-transport-expired"} {
		if !KnownReadStep(step) {
			t.Fatal(step)
		}
	}
	for _, bad := range []string{"helper-transport-", "helper-transport-other", "helper-transport-exec-1"} {
		if KnownReadStep(bad) {
			t.Fatal(bad)
		}
	}
	if transportStep(ErrPermission) != "helper-transport" || transportStep(transportError{"exec"}) != "helper-transport-exec" {
		t.Fatal("transport step naming changed")
	}
	if tlsRefusal(errors.New("dial failed")) || !tlsRefusal(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}) || !tlsRefusal(x509.HostnameError{}) {
		t.Fatal("only certificate and TLS verification failures are permanent")
	}
	if (transportError{"expired"}).transient() || !errors.Is(transportError{"deadline"}, ErrTransport) {
		t.Fatal("expiry must not be retried and causes must keep ErrTransport")
	}
}

// A transient failure of the respond exchange is never repeated: the helper
// may already have published, and a second respond could duplicate it.
func TestPublicationIsNeverRepeatedOnTransientFailures(t *testing.T) {
	for _, kind := range []string{"dropped", "unavailable", "relay"} {
		t.Run(kind, func(t *testing.T) {
			binding, pod, reply := bindingFixture(t)
			var execs atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/exec") {
					json.NewEncoder(w).Encode(pod)
					return
				}
				execs.Add(1)
				switch kind {
				case "dropped":
					dropExec(t, w, r)
				case "unavailable":
					unavailableExec(w, 503, "ServiceUnavailable")
				case "relay":
					relayFailure(t, w, r)
				}
			}))
			defer server.Close()
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			client, err := NewClient(Config{APIServer: server.URL, CA: ca, BearerToken: strings.Repeat("x", 32)})
			if err != nil {
				t.Fatal(err)
			}
			session, err := Bind(context.Background(), client, binding)
			if err != nil {
				t.Fatal(err)
			}
			err = session.Publish(context.Background(), reply)
			var stage interface{ PublicationStage() string }
			if !errors.Is(err, ErrTransport) || !errors.As(err, &stage) || stage.PublicationStage() != "helper-transport" {
				t.Fatalf("publication stop changed: %v", err)
			}
			if execs.Load() != 1 {
				t.Fatalf("respond repeated: %d exchanges", execs.Load())
			}
		})
	}
}
