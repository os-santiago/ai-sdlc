package podtransport

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func runtimeClassServer(t *testing.T, handler http.HandlerFunc) (*Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 32) || !strings.HasPrefix(r.URL.Path, "/apis/node.k8s.io/v1/runtimeclasses/") || r.URL.RawQuery != "" {
			t.Error("unexpected runtime class request", r.Method, r.URL.String())
			w.WriteHeader(400)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(Config{APIServer: server.URL, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), BearerToken: strings.Repeat("x", 32)})
	if err != nil {
		t.Fatal(err)
	}
	return client, &calls
}

func TestCheckRuntimeClassAcceptsAnyPlatformSandboxClass(t *testing.T) {
	for _, name := range []string{"ai-sdlc-offline", "gvisor", "kata", "kata-mshv-vm-isolation"} {
		client, calls := runtimeClassServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/apis/node.k8s.io/v1/runtimeclasses/"+name {
				w.WriteHeader(404)
				return
			}
			w.Write([]byte(`{"kind":"RuntimeClass","apiVersion":"node.k8s.io/v1","metadata":{"name":"` + name + `","uid":"u","resourceVersion":"1","managedFields":[]},"handler":"runsc","scheduling":{"nodeSelector":{"sandbox":"true"}}}`))
		})
		if err := client.CheckRuntimeClass(context.Background(), name); err != nil {
			t.Fatal(name, err)
		}
		if calls.Load() != 1 {
			t.Fatal("preflight is not one read")
		}
	}
}

func TestCheckRuntimeClassFailsClosed(t *testing.T) {
	cases := map[string]func(http.ResponseWriter, *http.Request){
		"missing":   func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) },
		"forbidden": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) },
		"server":    func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) },
		"other name": func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"kind":"RuntimeClass","metadata":{"name":"runc"},"handler":"runc"}`))
		},
		"no handler": func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"kind":"RuntimeClass","metadata":{"name":"kata"}}`))
		},
		"wrong kind": func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"kind":"Status","metadata":{"name":"kata"},"handler":"kata"}`))
		},
		"terminating": func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"kind":"RuntimeClass","metadata":{"name":"kata","deletionTimestamp":"2026-01-01T00:00:00Z"},"handler":"kata"}`))
		},
		"malformed": func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"kind":`)) },
		"oversized": func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"kind":"RuntimeClass","metadata":{"name":"kata","annotations":{"a":"` + strings.Repeat("x", maxRuntimeClassBytes) + `"}},"handler":"kata"}`))
		},
	}
	for label, handler := range cases {
		client, _ := runtimeClassServer(t, handler)
		if err := client.CheckRuntimeClass(context.Background(), "kata"); !errors.Is(err, ErrRuntimeClassUnavailable) {
			t.Fatal(label, "accepted")
		}
	}
	// Invalid names never reach the API server.
	client, calls := runtimeClassServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	for _, name := range []string{"", "../namespaces", "kata?x=1", "Kata"} {
		if err := client.CheckRuntimeClass(context.Background(), name); !errors.Is(err, ErrRuntimeClassUnavailable) {
			t.Fatal("invalid name accepted", name)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid name was sent")
	}
	var nilClient *Client
	if nilClient.CheckRuntimeClass(context.Background(), "kata") == nil {
		t.Fatal("nil client accepted")
	}
}
