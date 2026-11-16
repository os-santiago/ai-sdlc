package execution

import (
	"context"
	"encoding/binary"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Lose exactly the selected real server COMMIT acknowledgement. Reconnection
// stays healthy so the controller's independent cleanup can be observed. This
// proxy carries only isolated fixture credentials and never logs wire bytes.
func executionCommitFault(t *testing.T, nth int32, role ...string) (*store.DB, <-chan struct{}) {
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
	component := "worker"
	if len(role) == 1 && role[0] == "verifier" {
		component = "verifier"
	} else if len(role) == 1 && role[0] == "release-manager" {
		component = "release_manager"
	} else if len(role) == 1 && role[0] == "build-controller" {
		component = "build_controller"
	} else if len(role) == 1 && role[0] == "deployer" {
		component = "deployer"
	} else if len(role) != 0 {
		t.Fatal("unsupported fixture identity")
	}
	u.User = url.UserPassword("aisdlc_"+component, component+strings.Repeat("q", 40))
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

func TestExecutionLostInboxCommitIsDeduplicated(t *testing.T) {
	f := newFixture(t)
	healthy := f.db
	faulted, dropped := executionCommitFault(t, 1)
	f.db = faulted
	if _, err := f.accept(f.envelope(t, f.command)); err == nil {
		t.Fatal("lost acknowledgement reported success")
	}
	select {
	case <-dropped:
	default:
		t.Fatal("actual COMMIT acknowledgement was not dropped")
	}
	f.db = healthy
	duplicate, err := f.accept(f.envelope(t, f.command))
	if err != nil || !duplicate {
		t.Fatalf("recovery duplicate=%v err=%v", duplicate, err)
	}
	if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='queued'`) != 1 {
		t.Fatal("replay duplicated request")
	}
}
func TestExecutionLostClaimCommitNeverAuthorizesEffects(t *testing.T) {
	f := newFixture(t)
	if _, err := f.accept(f.envelope(t, f.command)); err != nil {
		t.Fatal(err)
	}
	faulted, dropped := executionCommitFault(t, 1)
	claim, err := Claim(context.Background(), faulted, f.readyPolicy, f.command.ID())
	if err == nil || claim.Token != "" || claim.ID != "" {
		t.Fatal("uncertain claim exposed capability")
	}
	select {
	case <-dropped:
	default:
		t.Fatal("actual COMMIT acknowledgement was not dropped")
	}
	if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='stopped' AND stop_reason='uncertain'`) != 1 {
		t.Fatal("uncertain claim was not stopped after healthy reconnect")
	}
	if _, err := Claim(context.Background(), f.db, f.readyPolicy, f.command.ID()); err == nil {
		t.Fatal("stopped command was reclaimed")
	}
}
