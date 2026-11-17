package podtransport

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type cleanupRaceGate struct {
	denied  atomic.Bool
	records int
}

func (g *cleanupRaceGate) CheckCleanup(ctx context.Context) error {
	if ctx.Err() != nil || g.denied.Load() {
		return ErrTransport
	}
	return nil
}
func (g *cleanupRaceGate) RecordCleanup(ctx context.Context) error {
	if g.CheckCleanup(ctx) != nil {
		return ErrTransport
	}
	g.records++
	return nil
}

func TestBrowserCleanupRechecksAfterReadAndLostDeleteAcknowledgment(t *testing.T) {
	for _, mode := range []string{"gate-changes-after-pod-read", "namespace-changes-after-pod-read", "cluster-changes-after-pod-read", "lost-delete-ack", "scope-changes-at-absence"} {
		t.Run(mode, func(t *testing.T) {
			profile, request, route, _, _ := browserFixture(t)
			// Cleanup owns only resource containment; the browser request is already expired.
			request.IssuedAt = time.Now().Add(-time.Minute).UnixMilli()
			request.ExpiresAt = time.Now().Add(-40 * time.Second).UnixMilli()
			pod, err := CompileBrowserPod(profile, request, route, time.UnixMilli(request.IssuedAt))
			if err != nil {
				t.Fatal(err)
			}
			uid := consoleUID(50)
			pod.UID = types.UID(uid)
			var podRead, absent atomic.Bool
			var deletes atomic.Int32
			gate := &cleanupRaceGate{}
			api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var obj any
				switch {
				case r.URL.Path == "/api/v1/namespaces/kube-system":
					id := profile.ClusterUID
					if mode == "cluster-changes-after-pod-read" && podRead.Load() {
						id = consoleUID(99)
					}
					obj = core.Namespace{ObjectMeta: meta.ObjectMeta{Name: "kube-system", UID: types.UID(id)}}
				case r.URL.Path == "/api/v1/namespaces/"+browsercheck.Namespace:
					id := profile.NamespaceUID
					if mode == "namespace-changes-after-pod-read" && podRead.Load() || mode == "scope-changes-at-absence" && absent.Load() {
						id = consoleUID(99)
					}
					obj = core.Namespace{ObjectMeta: meta.ObjectMeta{Name: browsercheck.Namespace, UID: types.UID(id)}}
				case r.URL.Path == "/api/v1/namespaces/"+browsercheck.Namespace+"/pods/"+pod.Name:
					if r.Method == http.MethodDelete {
						var options meta.DeleteOptions
						if json.NewDecoder(r.Body).Decode(&options) != nil || options.Preconditions == nil || options.Preconditions.UID == nil || string(*options.Preconditions.UID) != uid {
							w.WriteHeader(409)
							return
						}
						deletes.Add(1)
						absent.Store(true)
						// The effect happened but the acknowledgement was lost. Never resend it.
						if mode == "lost-delete-ack" {
							w.WriteHeader(503)
							return
						}
						obj = meta.Status{Status: "Success"}
					} else {
						podRead.Store(true)
						if mode == "gate-changes-after-pod-read" {
							gate.denied.Store(true)
						}
						if mode == "scope-changes-at-absence" {
							absent.Store(true)
						}
						if absent.Load() {
							w.WriteHeader(404)
							return
						}
						obj = pod
					}
				default:
					w.WriteHeader(404)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(obj)
			}))
			defer api.Close()
			client, err := NewClient(Config{APIServer: api.URL, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw}), BearerToken: strings.Repeat("x", 40)})
			if err != nil {
				t.Fatal(err)
			}
			cleaner, err := NewBrowserCleanupClient(client)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err = cleaner.CleanupBrowser(ctx, profile, request, route, uid, gate)
			if err == nil || gate.records != 0 {
				t.Fatal("uncertain cleanup recorded as completed", err)
			}
			if mode == "lost-delete-ack" {
				if deletes.Load() != 1 {
					t.Fatal("original deletion not attempted once")
				}
				fresh, stop := context.WithTimeout(context.Background(), 2*time.Second)
				defer stop()
				err = cleaner.CleanupBrowser(fresh, profile, request, route, uid, gate)
				if err != nil || gate.records != 1 || deletes.Load() != 1 {
					t.Fatal("absence recovery failed or replayed DELETE", err)
				}
			} else if deletes.Load() != 0 {
				t.Fatal("DELETE issued after authority/scope changed")
			}
		})
	}
}
