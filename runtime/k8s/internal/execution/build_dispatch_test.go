package execution

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func buildDispatchDB(t *testing.T, b buildFixture) *store.DB {
	t.Helper()
	u, e := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	u.User = url.UserPassword("aisdlc_release_manager", b.passwords["release-manager"])
	db, e := store.Open(context.Background(), u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestBuildDispatchSignedOutboxToInboxAndNoRedispatch(t *testing.T) {
	b := newBuildFixture(t)
	db := buildDispatchDB(t, b)
	ctx := context.Background()
	load := func() (building.Configuration, authority.Identity, error) {
		return *b.cfg, b.f.Material.Identities["release-manager"], nil
	}
	sent, e := buildflow.DispatchMerged(ctx, db, load)
	if e != nil || !sent {
		t.Fatal("dispatch", e)
	}
	var payload, raw []byte
	if e = db.Pool.QueryRowContext(ctx, `SELECT payload,receipt FROM authorized_outbox WHERE operation='build-merged'`).Scan(&payload, &raw); e != nil {
		t.Fatal(e)
	}
	var signed authority.Receipt
	if authority.DecodeStrict(raw, &signed) != nil {
		t.Fatal("receipt")
	}
	c, e := building.Verify(b.cfg.Inner, b.cfg.Outer, signed, payload, time.Now())
	if e != nil || c.MergeID != b.command.MergeID || c.SourceSHA != b.command.SourceSHA || c.TreeSHA != b.command.TreeSHA {
		t.Fatal("immutable merged source", e)
	}
	transport, e := b.f.Material.Identities["relay"].Carry(b.cfg.Outer, signed, http.MethodPost, "/internal/build-merged", time.Now())
	if e != nil {
		t.Fatal(e)
	}
	in := delivery.Inbox{DB: b.journal.DB, Target: "build-controller"}
	if _, _, e = in.Accept(ctx, b.cfg.Outer, delivery.Envelope{Receipt: signed, Transport: transport, Payload: payload}, "build-merged", http.MethodPost, "/internal/build-merged", time.Now(), buildflow.InboxEffect(b.journal)); e != nil {
		t.Fatal("signed inbox", e)
	}
	if id, e := b.journal.NextBuild(ctx); e != nil || id != c.ID() {
		t.Fatal("persisted queue", e)
	}
	// A lost caller acknowledgement cannot mint another time-dependent command.
	if sent, e = buildflow.DispatchMerged(ctx, db, load); e != nil || sent {
		t.Fatal("redispatched original merge", e)
	}
	var count int
	if e := b.f.Admin.Pool.QueryRowContext(ctx, `SELECT count(*) FROM build_handoffs`).Scan(&count); e != nil || count != 1 {
		t.Fatal("handoff count", e)
	}
	if _, e = db.Pool.ExecContext(ctx, `DELETE FROM build_handoffs`); e == nil {
		t.Fatal("sender erased immutable handoff")
	}
}

func TestBuildDispatchRejectsStaleOrChangedAuthority(t *testing.T) {
	for _, scenario := range []string{"inner-freeze", "outer-freeze", "stale-issue", "stale-cursor", "policy-during-load", "wrong-role", "completed-work"} {
		t.Run(scenario, func(t *testing.T) {
			b := newBuildFixture(t)
			db := buildDispatchDB(t, b)
			ctx := context.Background()
			switch scenario {
			case "inner-freeze":
				b.cfg.Inner.Freeze = true
			case "outer-freeze":
				b.cfg.Outer.Freeze = true
			case "stale-issue":
				if _, e := b.f.Admin.Pool.ExecContext(ctx, `UPDATE intake_observations SET observed_at=clock_timestamp()-interval '10 minutes'`); e != nil {
					t.Fatal(e)
				}
			case "stale-cursor":
				if _, e := b.f.Admin.Pool.ExecContext(ctx, `UPDATE intake_cursors SET watermark=clock_timestamp()-interval '10 minutes'`); e != nil {
					t.Fatal(e)
				}
			case "completed-work":
				if _, e := b.f.Admin.Pool.ExecContext(ctx, `UPDATE work_items SET status='done'`); e != nil {
					t.Fatal(e)
				}
			}
			calls := 0
			load := func() (building.Configuration, authority.Identity, error) {
				calls++
				cfg := *b.cfg
				key := b.f.Material.Identities["release-manager"]
				if scenario == "policy-during-load" && calls > 1 {
					cfg.Outer.Freeze = true
				}
				if scenario == "wrong-role" {
					key = b.f.Material.Identities["worker"]
				}
				return cfg, key, nil
			}
			if sent, _ := buildflow.DispatchMerged(ctx, db, load); sent {
				t.Fatal("unauthorized dispatch")
			}
			var count int
			if e := b.f.Admin.Pool.QueryRowContext(ctx, `SELECT count(*) FROM authorized_outbox WHERE operation='build-merged'`).Scan(&count); e != nil || count != 0 {
				t.Fatal("outbox escaped authority", e)
			}
			if scenario == "policy-during-load" && calls != 2 {
				t.Fatal("did not reach policy revalidation")
			}
		})
	}
}

func TestBuildDispatchRollbackAndConcurrentSend(t *testing.T) {
	for _, mode := range []string{"outbox-failure", "concurrent"} {
		t.Run(mode, func(t *testing.T) {
			b := newBuildFixture(t)
			db := buildDispatchDB(t, b)
			ctx := context.Background()
			load := func() (building.Configuration, authority.Identity, error) {
				return *b.cfg, b.f.Material.Identities["release-manager"], nil
			}
			if mode == "outbox-failure" {
				if _, e := b.f.Admin.Pool.ExecContext(ctx, `ALTER TABLE authorized_outbox ADD CONSTRAINT fixture_deny_build CHECK(operation<>'build-merged')`); e != nil {
					t.Fatal(e)
				}
				defer func() {
					if _, e := b.f.Admin.Pool.ExecContext(ctx, `ALTER TABLE authorized_outbox DROP CONSTRAINT fixture_deny_build`); e != nil {
						t.Error("remove isolated failure injection", e)
					}
				}()
				if sent, e := buildflow.DispatchMerged(ctx, db, load); e == nil || sent {
					t.Fatal("outbox failure accepted")
				}
				for _, table := range []string{"build_handoffs", "authorized_outbox"} {
					var count int
					query := `SELECT count(*) FROM build_handoffs`
					if table == "authorized_outbox" {
						query = `SELECT count(*) FROM authorized_outbox WHERE operation='build-merged'`
					}
					if e := b.f.Admin.Pool.QueryRowContext(ctx, query).Scan(&count); e != nil || count != 0 {
						t.Fatal("partial handoff committed", table, e)
					}
				}
				return
			}
			var sent atomic.Int32
			var group sync.WaitGroup
			start := make(chan struct{})
			for n := 0; n < 2; n++ {
				group.Go(func() {
					<-start
					if ok, e := buildflow.DispatchMerged(ctx, db, load); e == nil && ok {
						sent.Add(1)
					}
				})
			}
			close(start)
			group.Wait()
			if sent.Load() != 1 {
				t.Fatal("multiple or missing dispatch", sent.Load())
			}
			var count int
			if e := b.f.Admin.Pool.QueryRowContext(ctx, `SELECT count(*) FROM authorized_outbox WHERE operation='build-merged'`).Scan(&count); e != nil || count != 1 {
				t.Fatal("outbox deduplication", e)
			}
		})
	}
}
