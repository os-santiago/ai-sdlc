package podtransport

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	core "k8s.io/api/core/v1"
	protocol "k8s.io/apimachinery/pkg/util/remotecommand"
)

func workspaceBinding(t *testing.T) (Binding, core.Pod, []byte) {
	t.Helper()
	b, old, _ := bindingFixture(t)
	bundle := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: strings.Repeat("a", 40), Files: []workspacebundle.File{{Path: "source.txt", Mode: 0644, Data: []byte("pinned source\n")}}}
	raw, _ := workspacebundle.Encode(bundle)
	a, err := executor.ParseAssignment(b.Assignment, modelmailbox.Digest(b.Assignment), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	a.TreeSHA256, _ = bundle.TreeDigest()
	b.Assignment, _ = json.Marshal(a)
	w, err := executor.Compile(b.Namespace, b.WorkerImage, b.MailboxImage, b.Assignment, b.Profile, b.ADev, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(w.Pod)
	var pod core.Pod
	json.Unmarshal(encoded, &pod)
	applyDefaults(&pod.Spec)
	pod.Spec.NodeName = old.Spec.NodeName
	pod.UID = old.UID
	pod.Status = old.Status
	return b, pod, raw
}

func workspaceSession(t *testing.T, b Binding, pod core.Pod, observe func(*core.Pod, int32), exchange func(*http.Request, *websocket.Conn)) (*Session, *atomic.Int32) {
	t.Helper()
	var gets, execs atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 32) {
			t.Error("unexpected fixture identity")
			w.WriteHeader(401)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/exec") {
			copy := pod.DeepCopy()
			count := gets.Add(1)
			if observe != nil {
				observe(copy, count)
			}
			json.NewEncoder(w).Encode(copy)
			return
		}
		execs.Add(1)
		if r.Method != "GET" || r.URL.Query().Get("container") != "mailbox" || r.URL.Query().Get("tty") != "false" {
			t.Error("arbitrary helper transport")
			w.WriteHeader(400)
			return
		}
		upgrade := websocket.Upgrader{Subprotocols: []string{protocol.StreamProtocolV5Name}}
		conn, err := upgrade.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		exchange(r, conn)
	}))
	t.Cleanup(server.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	client, err := NewClient(Config{APIServer: server.URL, CA: ca, BearerToken: strings.Repeat("x", 32)})
	if err != nil {
		t.Fatal(err)
	}
	session, err := Bind(context.Background(), client, b)
	if err != nil {
		t.Fatal(err)
	}
	return session, &execs
}
func workspaceReply(conn *websocket.Conn, out []byte, status bool) {
	if len(out) > 0 {
		conn.WriteMessage(websocket.BinaryMessage, append([]byte{1}, out...))
	}
	if status {
		conn.WriteMessage(websocket.BinaryMessage, append([]byte{3}, []byte(`{"status":"Success"}`)...))
	}
	conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
}

func TestWorkerExitObservationNeverInvokesHelperOrAdoptsReplacement(t *testing.T) {
	for _, mode := range []string{"running", "terminated", "replaced", "restarted", "expired"} {
		t.Run(mode, func(t *testing.T) {
			b, pod, _ := workspaceBinding(t)
			session, execs := workspaceSession(t, b, pod, func(p *core.Pod, count int32) {
				if count <= 1 {
					return
				}
				switch mode {
				case "terminated":
					p.Status.ContainerStatuses[0].State = core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 7}}
				case "replaced":
					p.UID = "99999999-9999-4999-8999-999999999999"
				case "restarted":
					p.Status.ContainerStatuses[0].RestartCount = 1
				}
			}, func(*http.Request, *websocket.Conn) { t.Error("observation invoked helper") })
			if mode == "expired" {
				session.expires = time.Now().Add(-time.Second)
			}
			code, exited, err := session.WorkerExit(context.Background())
			if mode == "running" && (err != nil || exited) {
				t.Fatal("running observation")
			}
			if mode == "terminated" && (err != nil || !exited || code != 7) {
				t.Fatal("original worker exit observation")
			}
			if mode != "running" && mode != "terminated" && err == nil {
				t.Fatal("invalid identity/authority observed")
			}
			if execs.Load() != 0 {
				t.Fatal("helper side effect")
			}
		})
	}
}
func sourceInput(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	var input []byte
	for {
		_, frame, err := conn.ReadMessage()
		if err != nil {
			t.Error("input channel failed")
			return nil
		}
		if bytes.Equal(frame, []byte{255, 0}) {
			return input
		}
		if len(frame) == 0 || frame[0] != 0 {
			t.Error("invalid stdin channel")
			return nil
		}
		input = append(input, frame[1:]...)
		if len(input) > workspacebundle.MaxWireBytes {
			t.Error("input exceeded bound")
			return nil
		}
	}
}
func TestWorkspaceImportBindsSourceTreeReceiptAndPod(t *testing.T) {
	for _, kind := range []string{"success", "wrong-source", "wrong-tree", "missing-receipt", "false-import", "null-count", "wrong-count", "wrong-digest", "unknown-field", "no-status", "replaced-after"} {
		t.Run(kind, func(t *testing.T) {
			b, pod, source := workspaceBinding(t)
			sent := append([]byte(nil), source...)
			if kind == "wrong-source" {
				sent = bytes.Replace(sent, []byte(strings.Repeat("a", 40)), []byte(strings.Repeat("c", 40)), 1)
			}
			if kind == "wrong-tree" {
				bundle, _ := workspacebundle.Decode(source)
				bundle.Files[0].Data = []byte("substituted")
				sent, _ = workspacebundle.Encode(bundle)
			}
			session, calls := workspaceSession(t, b, pod, func(p *core.Pod, n int32) {
				if kind == "replaced-after" && n == 3 {
					p.UID = "33333333-3333-4333-8333-333333333333"
				}
			}, func(r *http.Request, conn *websocket.Conn) {
				expected := []string{"/workspace-transfer", "import", "--pod-uid", b.PodUID, "--source-sha", strings.Repeat("a", 40), "--digest", workspacebundle.Digest(source)}
				if !reflect.DeepEqual(r.URL.Query()["command"], expected) || r.URL.Query().Get("stdin") != "true" {
					t.Error("unbound source import")
				}
				if !bytes.Equal(sourceInput(t, conn), source) {
					t.Error("source changed in transport")
				}
				receipt := map[string]any{"imported": true, "files": 1, "sourceSHA": strings.Repeat("a", 40), "bundleSHA256": workspacebundle.Digest(source)}
				switch kind {
				case "false-import":
					receipt["imported"] = false
				case "null-count":
					receipt["files"] = nil
				case "wrong-count":
					receipt["files"] = 2
				case "wrong-digest":
					receipt["bundleSHA256"] = strings.Repeat("d", 64)
				case "unknown-field":
					receipt["extra"] = true
				}
				raw, _ := json.Marshal(receipt)
				if kind == "missing-receipt" {
					raw = nil
				}
				workspaceReply(conn, raw, kind != "no-status")
			})
			err := session.ImportSource(context.Background(), sent)
			if (kind == "success") != (err == nil) {
				t.Fatalf("unexpected import result %v", err)
			}
			expected := int32(1)
			if kind == "wrong-source" || kind == "wrong-tree" {
				expected = 0
			}
			if calls.Load() != expected {
				t.Fatal("invalid source reached helper or transport retried")
			}
		})
	}
}
func TestWorkspaceStartUsesOnlyBoundMarker(t *testing.T) {
	for _, kind := range []string{"success", "stdout", "no-status", "replaced-after", "exited-after"} {
		t.Run(kind, func(t *testing.T) {
			b, pod, _ := workspaceBinding(t)
			a, _ := executor.ParseAssignment(b.Assignment, modelmailbox.Digest(b.Assignment), time.Now())
			session, calls := workspaceSession(t, b, pod, func(p *core.Pod, n int32) {
				if n == 3 && kind == "replaced-after" {
					p.UID = "33333333-3333-4333-8333-333333333333"
				}
				if n == 3 && kind == "exited-after" {
					p.Status.ContainerStatuses[0].State = core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 1}}
				}
			}, func(r *http.Request, conn *websocket.Conn) {
				expected := []string{"/executor-control", "start", b.PodUID, modelmailbox.Digest(b.Assignment), a.TreeSHA256}
				if !reflect.DeepEqual(r.URL.Query()["command"], expected) || r.URL.Query().Get("stdin") != "" {
					t.Error("unbound start helper")
				}
				var out []byte
				if kind == "stdout" {
					out = []byte("unexpected")
				}
				workspaceReply(conn, out, kind != "no-status")
			})
			err := session.Start(context.Background())
			if (kind == "success" || kind == "exited-after") != (err == nil) {
				t.Fatalf("unexpected start result %v", err)
			}
			if calls.Load() != 1 {
				t.Fatal("start transport replayed")
			}
		})
	}
}
func TestCandidateExportRequiresStableTerminationAndBoundedBundle(t *testing.T) {
	for _, kind := range []string{"success", "nonzero-exit", "still-running", "revived-after", "changed-exit-after", "replaced-after", "wrong-source", "malformed", "oversized", "no-status"} {
		t.Run(kind, func(t *testing.T) {
			b, pod, source := workspaceBinding(t)
			session, calls := workspaceSession(t, b, pod, func(p *core.Pod, n int32) {
				if n >= 2 && kind != "still-running" {
					code := int32(0)
					if kind == "nonzero-exit" || n == 3 && kind == "changed-exit-after" {
						code = 1
					}
					p.Status.ContainerStatuses[0].State = core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: code}}
				}
				if n == 3 && kind == "revived-after" {
					p.Status.ContainerStatuses[0].State = core.ContainerState{Running: &core.ContainerStateRunning{}}
				}
				if n == 3 && kind == "replaced-after" {
					p.UID = "33333333-3333-4333-8333-333333333333"
				}
			}, func(r *http.Request, conn *websocket.Conn) {
				expected := []string{"/workspace-transfer", "export", "--pod-uid", b.PodUID, "--source-sha", strings.Repeat("a", 40)}
				if !reflect.DeepEqual(r.URL.Query()["command"], expected) || r.URL.Query().Get("stdin") != "" {
					t.Error("unbound export helper")
				}
				out := source
				if kind == "wrong-source" {
					out = bytes.Replace(out, []byte(strings.Repeat("a", 40)), []byte(strings.Repeat("c", 40)), 1)
				}
				if kind == "malformed" {
					out = []byte(`{"version":`)
				}
				if kind == "oversized" {
					out = bytes.Repeat([]byte("x"), workspacebundle.MaxWireBytes+1)
				}
				workspaceReply(conn, out, kind != "no-status")
			})
			candidate, err := session.ExportCandidate(context.Background())
			success := kind == "success" || kind == "nonzero-exit"
			if success != (err == nil) {
				t.Fatalf("unexpected candidate result %v", err)
			}
			if success {
				if !bytes.Equal(candidate.Bytes(), source) {
					t.Fatal("export changed")
				}
				copy := candidate.Bytes()
				copy[0] = 'x'
				if bytes.Equal(candidate.Bytes(), copy) {
					t.Fatal("candidate bytes mutable")
				}
				if (kind == "nonzero-exit") != (candidate.WorkerExitCode() != 0) {
					t.Fatal("exit status lost")
				}
			}
			expected := int32(1)
			if kind == "still-running" {
				expected = 0
			}
			if calls.Load() != expected {
				t.Fatal("live writer exported or transport retried")
			}
		})
	}
}

func TestWorkspaceExpiryDuringObservationPreventsStartEffect(t *testing.T) {
	b, pod, _ := workspaceBinding(t)
	session, calls := workspaceSession(t, b, pod, func(_ *core.Pod, n int32) {
		if n == 2 {
			time.Sleep(120 * time.Millisecond)
		}
	}, func(_ *http.Request, conn *websocket.Conn) { workspaceReply(conn, nil, true) })
	session.expires = time.Now().Add(50 * time.Millisecond)
	if session.Start(context.Background()) == nil {
		t.Fatal("expired start accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("expired assignment reached mutating helper")
	}
}

func TestMailboxReadSeparatesProtocolRejectionFromWorkerExit(t *testing.T) {
	for _, which := range []string{"valid-exit-race", "invalid-uid-exit-race", "helper-failure-exit-race"} {
		t.Run(which, func(t *testing.T) {
			b, pod, _ := workspaceBinding(t)
			request := modelmailbox.Envelope{Version: modelmailbox.Version, Kind: "request", Attempt: "fixture", PodUID: b.PodUID, Sequence: 1, ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), Payload: []byte(`{"model":"fixture","messages":[{"role":"user","content":"bounded"}],"max_tokens":1}`)}
			a, err := executor.ParseAssignment(b.Assignment, modelmailbox.Digest(b.Assignment), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			request.Attempt = fmt.Sprintf("%s-%d", a.WorkItemID, a.Attempt)
			request.RequestSHA256 = modelmailbox.Digest(request.Payload)
			if which == "invalid-uid-exit-race" {
				request.PodUID = "11111111-1111-4111-8111-111111111111"
			}
			raw, _ := json.Marshal(request)
			session, _ := workspaceSession(t, b, pod, func(p *core.Pod, n int32) {
				if n >= 3 {
					for i := range p.Status.ContainerStatuses {
						if p.Status.ContainerStatuses[i].Name == "worker" {
							p.Status.ContainerStatuses[i].State = core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}}
						}
					}
				}
			}, func(_ *http.Request, conn *websocket.Conn) {
				workspaceReply(conn, raw, which != "helper-failure-exit-race")
			})
			observed, readErr := session.ReadRequest(context.Background())
			err = readErr
			if which == "valid-exit-race" {
				if observed.Sequence != request.Sequence || !bytes.Equal(observed.Payload, request.Payload) {
					t.Fatal("observed request lost on exit")
				}
				if !errors.Is(err, ErrWorkerExited) {
					t.Fatal("valid original exit not distinguished", err)
				}
			} else if !errors.Is(err, ErrTransport) || errors.Is(err, ErrWorkerExited) {
				t.Fatal("protocol rejection became worker exit", err)
			}
		})
	}
}
