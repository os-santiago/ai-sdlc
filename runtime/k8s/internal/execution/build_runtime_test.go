package execution

import (
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func TestBuildLeadershipLossDuringPostKeepsQuarantineAndLateACK(t *testing.T) {
	b := newBuildFixture(t)
	claim := buildClaim(t, b)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	posted := make(chan struct{})
	reply := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(reply) }) }
	var posts, loads atomic.Int32
	uid := store.NewID()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
			close(posted)
			<-reply
			fmt.Fprint(w, uid)
			return
		}
		w.WriteHeader(500)
	}))
	defer server.Close()
	defer release()
	client, e := podtransport.NewClient(podtransport.Config{APIServer: server.URL, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), BearerToken: strings.Repeat("x", 32)})
	if e != nil {
		t.Fatal(e)
	}
	runtime := buildflow.Runtime{DB: b.journal.DB, Load: func() (buildflow.Installed, error) {
		loads.Add(1)
		return buildflow.Installed{}, buildflow.ErrBuildFlow
	}, Client: func() (*podtransport.Client, error) { return client, nil }}
	done := make(chan error, 1)
	go func() {
		_, e := b.journal.DB.WithBuildController(ctx, func(active context.Context) error {
			effect, e := b.journal.BeginBuildEffect(active, claim, b.plan(), b.compiled.Retained)
			if e != nil {
				return e
			}
			// Simulate a POST already accepted by the API whose original response
			// arrives after the controller loses its PostgreSQL session.
			httpClient := server.Client()
			httpClient.Timeout = 8 * time.Second
			request, e := http.NewRequestWithContext(context.WithoutCancel(active), "POST", server.URL, nil)
			if e != nil {
				return e
			}
			response, e := httpClient.Do(request)
			if e != nil {
				return e
			}
			response.Body.Close()
			if e = b.journal.RememberBuildCreated(context.WithoutCancel(active), effect, uid); e != nil {
				return e
			}
			if b.journal.CheckBuild(context.WithoutCancel(active), claim) == nil {
				return fmt.Errorf("terminal claim renewed")
			}
			if _, e = b.journal.BeginPublishEffect(context.WithoutCancel(active), claim); e == nil {
				return fmt.Errorf("publisher escaped terminal claim")
			}
			return active.Err()
		})
		done <- e
	}()
	select {
	case <-posted:
	case <-ctx.Done():
		t.Fatal("POST not reached")
	}
	const lockID int64 = 0x414953444C434255
	var terminated bool
	if e = b.f.Admin.Pool.QueryRowContext(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype='advisory' AND classid=$1::oid AND objid=$2::oid AND objsubid=1 AND granted`, lockID>>32, lockID&0xffffffff).Scan(&terminated); e != nil || !terminated {
		t.Fatal("session loss injection", e)
	}
	state, e := runtime.PollOnce(ctx)
	if e == nil || state != "build-containment" || loads.Load() != 0 || posts.Load() != 1 {
		t.Fatal("new leader escaped unknown-create barrier", state, e)
	}
	original, e := b.journal.ReadBuildCleanup(ctx, claim)
	if e != nil || !original.UnknownCreate {
		t.Fatal("unknown intent was lost", e)
	}
	release()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("lost session returned success")
		}
	case <-ctx.Done():
		t.Fatal("old callback failed to stop")
	}
	original, e = b.journal.ReadBuildCleanup(ctx, claim)
	if e != nil || original.UnknownCreate || len(original.Resources) != 1 || original.Resources[0].UID != uid {
		t.Fatal("late UID not retained exclusively for containment", e)
	}
	var cleanups int
	if e = b.f.Admin.Pool.QueryRowContext(ctx, `SELECT count(*) FROM build_cleanups`).Scan(&cleanups); e != nil || cleanups != 0 {
		t.Fatal("unknown creation acknowledged as absent", e)
	}
}
