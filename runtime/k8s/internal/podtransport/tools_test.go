package podtransport

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	protocol "k8s.io/apimachinery/pkg/util/remotecommand"
)

// The tool helpers run only the fixed read-tool and respond-tool modes for the
// bound attempt, accept only a valid tool request, report "none pending", and
// confirm a publication only by a receipt naming the request digest.
func TestToolHelpersAreFixedBoundAndConfirmed(t *testing.T) {
	for _, kind := range []string{"read", "pending", "read-wrong-kind", "respond", "respond-wrong-receipt", "respond-refused", "respond-not-pending"} {
		t.Run(kind, func(t *testing.T) {
			binding, pod, reply := bindingFixture(t)
			payload := []byte(`{"tool":"build_test","target":"rest-villains","diff":""}`)
			request := modelmailbox.Envelope{Version: modelmailbox.Version, Kind: "tool-request", Attempt: reply.Attempt, PodUID: reply.PodUID, Sequence: 1,
				RequestSHA256: modelmailbox.Digest(payload), ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), Payload: payload}
			response := request
			response.Kind, response.Status, response.Payload = "tool-response", 200, []byte(`{"text":"tests run 94, failed 0"}`)
			mode := "read-tool"
			if strings.HasPrefix(kind, "respond") {
				mode = "respond-tool"
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/exec") {
					json.NewEncoder(w).Encode(pod)
					return
				}
				q := r.URL.Query()
				if q.Get("container") != "mailbox" || q.Get("stdin") != map[bool]string{true: "true", false: ""}[mode == "respond-tool"] ||
					!reflect.DeepEqual(q["command"], []string{"/model-mailbox", mode, "--attempt", reply.Attempt, "--pod-uid", reply.PodUID}) {
					t.Error("arbitrary helper command", q)
					w.WriteHeader(400)
					return
				}
				conn, err := (&websocket.Upgrader{Subprotocols: []string{protocol.StreamProtocolV5Name}}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				if mode == "respond-tool" {
					conn.SetReadDeadline(time.Now().Add(time.Second))
					var input []byte
					for {
						_, frame, err := conn.ReadMessage()
						if err != nil || bytes.Equal(frame, []byte{255, 0}) {
							break
						}
						input = append(input, frame[1:]...)
					}
					if e, err := modelmailbox.Decode(input); err != nil || !bytes.Equal(e.Payload, response.Payload) {
						t.Error("exact tool response changed")
					}
				}
				var out []byte
				status := []byte(`{"status":"Success"}`)
				switch kind {
				case "read":
					out, _ = json.Marshal(request)
				case "pending":
					status = []byte(`{"status":"Failure","reason":"NonZeroExitCode","details":{"causes":[{"reason":"ExitCode","message":"3"}]}}`)
				case "read-wrong-kind":
					wrong := request
					wrong.Kind = "request"
					out, _ = json.Marshal(wrong)
				case "respond":
					out, _ = json.Marshal(map[string]string{"published": request.RequestSHA256})
				case "respond-wrong-receipt":
					out, _ = json.Marshal(map[string]string{"published": strings.Repeat("e", 64)})
				case "respond-refused":
					status = []byte(`{"status":"Failure","reason":"NonZeroExitCode","details":{"causes":[{"reason":"ExitCode","message":"1"}]}}`)
				case "respond-not-pending":
					status = []byte(`{"status":"Failure","reason":"NonZeroExitCode","details":{"causes":[{"reason":"ExitCode","message":"3"}]}}`)
				}
				if len(out) > 0 {
					_ = conn.WriteMessage(websocket.BinaryMessage, append([]byte{1}, out...))
				}
				_ = conn.WriteMessage(websocket.BinaryMessage, append([]byte{3}, status...))
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
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
			if mode == "read-tool" {
				got, err := session.ReadToolRequest(context.Background())
				switch kind {
				case "read":
					if err != nil || got.RequestSHA256 != request.RequestSHA256 || !bytes.Equal(got.Payload, payload) {
						t.Fatal("tool request", err)
					}
				case "pending":
					if !errors.Is(err, modelmailbox.ErrPending) {
						t.Fatal("pending not reported", err)
					}
				default:
					if err == nil {
						t.Fatal("non-tool envelope accepted")
					}
				}
				return
			}
			err = session.PublishToolResponse(context.Background(), response)
			if (kind == "respond") != (err == nil) || (kind == "respond-not-pending") != errors.Is(err, ErrToolNotPending) {
				t.Fatal("publication result", err)
			}
		})
	}
}
