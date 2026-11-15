package modelbroker

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// Lose exactly the selected real server COMMIT acknowledgement. Reconnection
// stays healthy so the dispatcher's independent cleanup can be observed. This
// proxy carries only isolated fixture credentials and never logs wire bytes.
func brokerCommitFault(t *testing.T, nth int32) (*store.DB, <-chan struct{}) {
	t.Helper()
	u, err := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("sslmode") != "disable" {
		t.Skip("plaintext PostgreSQL fixture required")
	}
	upstream := u.Host
	if u.Port() == "" {
		upstream = net.JoinHostPort(u.Hostname(), "5432")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var commits atomic.Int32
	dropped := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer client.Close()
				server, err := net.DialTimeout("tcp", upstream, 2*time.Second)
				if err != nil {
					return
				}
				defer server.Close()
				_ = client.SetDeadline(time.Now().Add(15 * time.Second))
				_ = server.SetDeadline(time.Now().Add(15 * time.Second))
				copied := make(chan struct{})
				go func() { _, _ = io.Copy(server, client); close(copied) }()
				defer func() { client.Close(); server.Close(); <-copied }()
				for {
					var header [5]byte
					if _, err = io.ReadFull(server, header[:]); err != nil {
						return
					}
					size := binary.BigEndian.Uint32(header[1:])
					if size < 4 || size > 1<<20 {
						return
					}
					body := make([]byte, int(size)-4)
					if _, err = io.ReadFull(server, body); err != nil {
						return
					}
					if header[0] == 'C' && string(body) == "COMMIT\x00" && commits.Add(1) == nth {
						close(dropped)
						return
					}
					if _, err = client.Write(header[:]); err != nil {
						return
					}
					if _, err = client.Write(body); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); wg.Wait() })
	u.Host = listener.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	db, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.Pool.SetMaxOpenConns(1)
	return db, dropped
}

func TestBrokerLostCommitStopsParentBeforeSiblingCanInvoke(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nth   int32
		state string
	}{
		{"reservation", 1, "stopped"}, {"claim", 2, "ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls, publications atomic.Int32
			d, a, profile, request := brokerFixture(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) }, publisherFunc(func(context.Context, modelmailbox.Envelope) error { publications.Add(1); return nil }))
			healthy := d.DB
			ctx := context.Background()
			sibling := a
			sibling.ID, sibling.PodUID, sibling.Attempt = store.NewID(), store.NewID(), 2
			if err := healthy.CreateModelAllocation(ctx, sibling); err != nil {
				t.Fatal(err)
			}
			reserved, err := healthy.ReserveModelCall(ctx, sibling.ID, sibling.PodUID, 1, request.Payload)
			if err != nil || reserved.Reason != "reserved" {
				t.Fatalf("sibling reserve: %+v %v", reserved, err)
			}
			faulted, dropped := brokerCommitFault(t, tc.nth)
			d.DB = faulted
			result, err := d.Deliver(ctx, a.ID, profile, request)
			if !errors.Is(err, ErrUncertain) || result.DispatchAttempted || result.HandoffConfirmed {
				t.Fatalf("uncertain commit: %+v %v", result, err)
			}
			select {
			case <-dropped:
			default:
				t.Fatal("real COMMIT ACK was not dropped")
			}
			parent, call, handed, count := ledgerState(t, healthy, a)
			if parent != "stopped" || call != tc.state || handed || count != 2 {
				t.Fatalf("cleanup: %s %s %t %d", parent, call, handed, count)
			}
			claimed, err := healthy.ClaimModelCall(ctx, reserved.Call.ModelCallRef)
			if err != nil || claimed.Invoke {
				t.Fatalf("sibling escaped stop: %+v %v", claimed, err)
			}
			if calls.Load() != 0 || publications.Load() != 0 {
				t.Fatal("uncertain commit invoked provider or publisher")
			}
			if result.Call.AllocationID != a.ID || result.Call.RequestSHA256 != request.RequestSHA256 {
				t.Fatal("lost deterministic reference")
			}
		})
	}
}
