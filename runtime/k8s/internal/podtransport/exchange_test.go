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
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	protocol "k8s.io/apimachinery/pkg/util/remotecommand"
)

func bindingFixture(t *testing.T) (Binding, core.Pod, modelmailbox.Envelope) {
	t.Helper()
	adev := []byte("synthetic profile fixture")
	profile, _ := json.Marshal(executor.Profile{Version: executor.ProfileVersion, Model: "fixture-model", MaxSteps: 4, MaxSeconds: 45, MaxTokens: 2048, ADevSHA256: modelmailbox.Digest(adev)})
	a := executor.Assignment{Version: executor.AssignmentVersion, WorkItemID: "11111111-1111-4111-8111-111111111111", Attempt: 1, Repository: "fixture/product", SourceSHA: strings.Repeat("a", 40), TreeSHA256: strings.Repeat("b", 64), ProfileSHA256: modelmailbox.Digest(profile), AuthorityRevision: strings.Repeat("c", 64), IssueNumbers: []int{1}, Title: "Component fixture", Body: "Synthetic issue only", AcceptanceCriteria: []string{"No live work"}, ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	assignment, _ := json.Marshal(a)
	b := Binding{Namespace: "fixture", WorkerImage: "fixture/worker@sha256:" + strings.Repeat("a", 64), MailboxImage: "fixture/mailbox@sha256:" + strings.Repeat("b", 64), PodUID: "22222222-2222-4222-8222-222222222222", Assignment: assignment, Profile: profile, ADev: adev}
	w, err := executor.Compile(b.Namespace, b.WorkerImage, b.MailboxImage, b.Assignment, b.Profile, b.ADev, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(w.Pod)
	var p core.Pod
	if json.Unmarshal(raw, &p) != nil {
		t.Fatal("fixture pod decode")
	}
	applyDefaults(&p.Spec)
	p.Spec.NodeName = "fixture-node"
	p.UID = types.UID(b.PodUID)
	p.Status.Phase = core.PodRunning
	for _, name := range []string{"worker", "mailbox"} {
		digest := strings.Repeat("a", 64)
		if name == "mailbox" {
			digest = strings.Repeat("b", 64)
		}
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, core.ContainerStatus{Name: name, ContainerID: "containerd://" + strings.Repeat("a", 64), Image: "fixture/" + name + ":imported", ImageID: "fixture/" + name + "@sha256:" + digest, State: core.ContainerState{Running: &core.ContainerStateRunning{}}})
	}
	reply := modelmailbox.Envelope{Version: modelmailbox.Version, Kind: "response", Attempt: a.WorkItemID + "-1", PodUID: b.PodUID, Sequence: 1, RequestSHA256: strings.Repeat("d", 64), Status: 200, ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), Payload: []byte(`{"choices":[{"message":{"role":"assistant","content":"fixture"}}]}`)}
	return b, p, reply
}

func receiptBytes(e modelmailbox.Envelope) []byte {
	raw, _ := json.Marshal(modelmailbox.PublicationReceipt{Version: modelmailbox.ReceiptVersion, Attempt: e.Attempt, PodUID: e.PodUID, Sequence: e.Sequence, RequestSHA256: e.RequestSHA256, ResponseSHA256: modelmailbox.Digest(e.Payload), Status: e.Status, Bytes: len(e.Payload)})
	return raw
}

func TestPublicationRequiresExplicitBoundedStatusAndReceipt(t *testing.T) {
	for _, kind := range []string{"success", "no-status", "no-receipt", "duplicate-status", "truncated-status", "wrong-receipt", "oversized-status", "oversized-stdout", "oversized-stderr", "too-many-frames", "unknown-channel", "text-frame", "abnormal-close", "helper-failure", "cancelled", "replaced-after", "permission-denied"} {
		t.Run(kind, func(t *testing.T) {
			binding, pod, reply := bindingFixture(t)
			var upgrades, gets atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 32) {
					t.Error("credential binding changed")
					w.WriteHeader(401)
					return
				}
				if !strings.HasSuffix(r.URL.Path, "/exec") {
					count := gets.Add(1)
					p := pod.DeepCopy()
					if kind == "replaced-after" && count == 3 {
						p.UID = "33333333-3333-4333-8333-333333333333"
					}
					json.NewEncoder(w).Encode(p)
					return
				}
				upgrades.Add(1)
				if kind == "permission-denied" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(403)
					w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403}`))
					return
				}
				q := r.URL.Query()
				if r.Method != "GET" || q.Get("container") != "mailbox" || q.Get("stdin") != "true" || q.Get("tty") != "false" || !reflect.DeepEqual(q["command"], []string{"/model-mailbox", "respond", "--attempt", reply.Attempt, "--pod-uid", reply.PodUID}) {
					t.Error("arbitrary helper command")
					w.WriteHeader(400)
					return
				}
				u := websocket.Upgrader{Subprotocols: []string{protocol.StreamProtocolV5Name}}
				conn, err := u.Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				conn.SetReadDeadline(time.Now().Add(time.Second))
				var input []byte
				for {
					_, frame, err := conn.ReadMessage()
					if err != nil {
						return
					}
					if bytes.Equal(frame, []byte{255, 0}) {
						break
					}
					if len(frame) == 0 || frame[0] != 0 {
						t.Error("stdin channel")
						return
					}
					input = append(input, frame[1:]...)
				}
				e, err := modelmailbox.Decode(input)
				if err != nil || !bytes.Equal(e.Payload, reply.Payload) {
					t.Error("exact input changed")
					return
				}
				if kind == "cancelled" {
					time.Sleep(150 * time.Millisecond)
					return
				}
				out := receiptBytes(reply)
				status := []byte(`{"status":"Success"}`)
				if kind == "no-receipt" {
					out = nil
				}
				if kind == "no-status" {
					status = nil
				}
				if kind == "duplicate-status" {
					status = append(status, status...)
				}
				if kind == "truncated-status" {
					status = []byte(`{"status":`)
				}
				if kind == "wrong-receipt" {
					out = bytes.Replace(out, []byte(reply.PodUID), []byte("33333333-3333-4333-8333-333333333333"), 1)
				}
				if kind == "oversized-status" {
					status = bytes.Repeat([]byte("x"), 8193)
				}
				if kind == "oversized-stdout" {
					out = bytes.Repeat([]byte("x"), modelmailbox.MaxReceiptBytes+1)
				}
				if kind == "helper-failure" {
					status = []byte(`{"status":"Failure","reason":"NonZeroExitCode","details":{"causes":[{"reason":"ExitCode","message":"1"}]}}`)
				}
				write := func(channel byte, data []byte) {
					_ = conn.WriteMessage(websocket.BinaryMessage, append([]byte{channel}, data...))
				}
				if kind == "oversized-stderr" {
					write(2, bytes.Repeat([]byte("x"), 8193))
				}
				if kind == "too-many-frames" {
					for range 1025 {
						write(1, nil)
					}
				}
				if kind == "unknown-channel" {
					write(42, []byte("unknown"))
				}
				if kind == "text-frame" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte("unexpected"))
				}
				// Fragments are legal on both channels; their cumulative bounds apply.
				write(1, out[:len(out)/2])
				write(1, out[len(out)/2:])
				write(3, status[:len(status)/2])
				write(3, status[len(status)/2:])
				code := websocket.CloseNormalClosure
				if kind == "abnormal-close" {
					code = websocket.CloseGoingAway
				}
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(time.Second))
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
			ctx := context.Background()
			if kind == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
				defer cancel()
			}
			start := time.Now()
			err = session.Publish(ctx, reply)
			if (kind == "success") != (err == nil) {
				t.Fatalf("result %v", err)
			}
			if kind == "permission-denied" && !errors.Is(err, ErrPermission) {
				t.Fatal("forbidden status was not recognized")
			}
			if upgrades.Load() != 1 || time.Since(start) > time.Second {
				t.Fatal("transport replayed or failed to bound fixture")
			}
		})
	}
}

func TestBindingRejectsChangedPodBeforeAnyExec(t *testing.T) {
	for _, kind := range []string{"uid", "deleting", "image", "privilege", "init", "ephemeral", "restart", "container-id", "annotation", "unknown-field", "initial-image"} {
		t.Run(kind, func(t *testing.T) {
			binding, pod, reply := bindingFixture(t)
			var gets, execs atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/exec") {
					execs.Add(1)
					w.WriteHeader(500)
					return
				}
				p := pod.DeepCopy()
				n := gets.Add(1)
				if kind == "initial-image" {
					p.Status.ContainerStatuses[1].ImageID = "fixture/old@sha256:" + strings.Repeat("c", 64)
				}
				if n > 1 {
					switch kind {
					case "uid":
						p.UID = "33333333-3333-4333-8333-333333333333"
					case "deleting":
						now := meta.Now()
						p.DeletionTimestamp = &now
					case "image":
						p.Spec.Containers[1].Image = "fixture:mutable"
					case "privilege":
						yes := true
						p.Spec.Containers[1].SecurityContext.Privileged = &yes
					case "init":
						p.Spec.InitContainers = []core.Container{{Name: "injected"}}
					case "ephemeral":
						p.Spec.EphemeralContainers = []core.EphemeralContainer{{}}
					case "restart":
						p.Status.ContainerStatuses[1].RestartCount = 1
					case "container-id":
						p.Status.ContainerStatuses[1].ContainerID = "containerd://" + strings.Repeat("b", 64)
					case "annotation":
						p.Annotations["injected"] = "true"
					}
				}
				raw, _ := json.Marshal(p)
				if n > 1 && kind == "unknown-field" {
					raw = bytes.Replace(raw, []byte(`"spec":{`), []byte(`"spec":{"unknownFuturePermission":true,`), 1)
				}
				w.Write(raw)
			}))
			defer server.Close()
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			c, err := NewClient(Config{APIServer: server.URL, CA: ca, BearerToken: strings.Repeat("x", 32)})
			if err != nil {
				t.Fatal(err)
			}
			s, err := Bind(context.Background(), c, binding)
			if kind == "initial-image" {
				if err == nil || execs.Load() != 0 {
					t.Fatal("previous container image accepted as baseline")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.Publish(context.Background(), reply) == nil || execs.Load() != 0 {
				t.Fatal("changed Pod reached helper")
			}
		})
	}
}

func TestPublicationNamesTheExitCodeOfAnAlreadyExitedWorker(t *testing.T) {
	for _, kind := range []string{"exited", "other-terminal-id", "restarted", "running-then-forbidden"} {
		t.Run(kind, func(t *testing.T) { publishToExitedWorker(t, kind) })
	}
}

func publishToExitedWorker(t *testing.T, kind string) {
	binding, pod, reply := bindingFixture(t)
	var gets, execs atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/exec") {
			execs.Add(1)
			w.WriteHeader(500)
			return
		}
		p := pod.DeepCopy()
		if gets.Add(1) > 1 {
			if kind == "running-then-forbidden" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			// The launcher's run budget ended while the call was in flight.
			for i := range p.Status.ContainerStatuses {
				if status := &p.Status.ContainerStatuses[i]; status.Name == "worker" {
					status.State = core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 124, ContainerID: status.ContainerID}}
					if kind == "other-terminal-id" {
						status.State.Terminated.ContainerID = "containerd://" + strings.Repeat("f", 64)
					}
					if kind == "restarted" {
						status.RestartCount = 1
					}
				}
			}
		}
		raw, _ := json.Marshal(p)
		w.Write(raw)
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	c, err := NewClient(Config{APIServer: server.URL, CA: ca, BearerToken: strings.Repeat("x", 32)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := Bind(context.Background(), c, binding)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Publish(context.Background(), reply)
	var exited interface{ WorkerExitCode() (int32, bool) }
	var stage interface{ PublicationStage() string }
	if err == nil || execs.Load() != 0 || !errors.As(err, &stage) || stage.PublicationStage() != "before-observation" {
		t.Fatalf("%s: publication not refused before any exec: %v", kind, err)
	}
	if gets.Load() != 2 {
		t.Fatalf("%s: %d pod observations, want one after binding", kind, gets.Load())
	}
	switch kind {
	case "exited":
		if !errors.As(err, &exited) || !errors.Is(err, ErrTransport) {
			t.Fatalf("exit code not carried: %v", err)
		}
		if code, ok := exited.WorkerExitCode(); !ok || code != 124 {
			t.Fatalf("exit code %d %v", code, ok)
		}
	case "running-then-forbidden":
		if errors.As(err, &exited) || !errors.Is(err, ErrPermission) {
			t.Fatalf("permission failure changed: %v", err)
		}
	default:
		// Only the pinned, unrestarted instance may report an exit code.
		if errors.As(err, &exited) || !errors.Is(err, ErrTransport) {
			t.Fatalf("%s: exit code from an unpinned instance: %v", kind, err)
		}
	}
}
