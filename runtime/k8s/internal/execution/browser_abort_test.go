package execution

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func unsentBrowserFixture(t *testing.T) (deploymentFixture, store.BrowserGate) {
	t.Helper()
	f := newDeploymentFixture(t)
	c := f.rawClaim(t)
	snapshot := f.before()
	route := f.health(releasing.Receipt{After: snapshot, Rollback: true}).Rollout
	request, e := store.BrowserRequest(c, f.p.cfg.Browser, "baseline", snapshot, route, time.Now().Add(25*time.Second))
	if e != nil {
		t.Fatal(e)
	}
	return f, store.BrowserGate{Journal: f.j, Claim: c, Phase: "baseline", Snapshot: snapshot, Rollout: route, Request: request}
}

func TestBrowserKnownNoSendConsumesOnlyOriginalAttempt(t *testing.T) {
	f, g := unsentBrowserFixture(t)
	ctx := context.Background()
	digest, nonce := strings.Repeat("a", 64), strings.Repeat("e", 64)
	if e := g.BeforeCreate(ctx, digest, nonce); e != nil {
		t.Fatal(e)
	}
	if pending, e := f.j.PendingBrowserCleanup(ctx); e != nil || !pending {
		t.Fatal("missing pending original send", pending, e)
	}
	for _, wrong := range []string{strings.Repeat("f", 64), ""} {
		if g.AbortBeforeSend(ctx, digest, wrong) == nil {
			t.Fatal("foreign attempt closed original send")
		}
		if g.AcknowledgePod(ctx, g.Request.RequestID, store.NewID(), wrong) == nil {
			t.Fatal("foreign attempt acknowledged a Pod")
		}
	}
	if e := g.AbortBeforeSend(ctx, digest, nonce); e != nil {
		t.Fatal("record known no-send", e)
	}
	if pending, e := f.j.PendingBrowserCleanup(ctx); e != nil || pending {
		t.Fatal("known no-send blocks later deployments", pending, e)
	}
	if pending, e := f.j.UnacknowledgedBrowserCreate(ctx); e != nil || pending {
		t.Fatal("known no-send treated as unknown CREATE", pending, e)
	}
	if cleanup, e := f.j.NextBrowserCleanup(ctx); e != nil || cleanup.Claim.ID != "" {
		t.Fatal("no-send acquired Pod cleanup", cleanup, e)
	}
	if g.BeforeCreate(ctx, digest, nonce) == nil || g.AcknowledgePod(ctx, g.Request.RequestID, store.NewID(), nonce) == nil {
		t.Fatal("consumed no-send attempt replayed")
	}
	if f.p.b.f.Count(t, `SELECT count(*) FROM deployment_browser_pods WHERE not_sent AND pod_uid IS NULL`) != 1 || f.p.b.f.Count(t, `SELECT count(*) FROM deployment_browser_evidence`) != 0 || f.p.b.f.Count(t, `SELECT count(*) FROM deployment_health`) != 0 {
		t.Fatal("no-send fabricated Pod or health")
	}
	for _, sql := range []string{`UPDATE deployment_browser_pods SET not_sent=false`, `DELETE FROM deployment_browser_pods`} {
		if _, e := f.j.Pool.Exec(sql); e == nil {
			t.Fatal("no-send outcome was mutable")
		}
	}
}

func TestBrowserCompetingAttemptCannotAbortWinner(t *testing.T) {
	f, g := unsentBrowserFixture(t)
	ctx := context.Background()
	digest := strings.Repeat("a", 64)
	nonces := []string{strings.Repeat("e", 64), strings.Repeat("f", 64)}
	type result struct {
		nonce string
		err   error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, nonce := range nonces {
		go func(n string) { <-start; results <- result{n, g.BeforeCreate(ctx, digest, n)} }(nonce)
	}
	close(start)
	a, b := <-results, <-results
	if (a.err == nil) == (b.err == nil) {
		t.Fatal("expected one original send owner", a.err, b.err)
	}
	winner, loser := a, b
	if winner.err != nil {
		winner, loser = b, a
	}
	if g.AbortBeforeSend(ctx, digest, loser.nonce) == nil {
		t.Fatal("loser aborted winner")
	}
	if pending, e := f.j.UnacknowledgedBrowserCreate(ctx); e != nil || !pending {
		t.Fatal("winner was released before ACK", pending, e)
	}
	if e := g.AcknowledgePod(ctx, g.Request.RequestID, store.NewID(), winner.nonce); e != nil {
		t.Fatal("winner cannot ACK", e)
	}
	if g.AbortBeforeSend(ctx, digest, winner.nonce) == nil {
		t.Fatal("ACK reclassified as no-send")
	}
}

func TestBrowserACKAndNoSendHaveOneImmutableOutcome(t *testing.T) {
	f, g := unsentBrowserFixture(t)
	ctx := context.Background()
	digest, nonce := strings.Repeat("a", 64), strings.Repeat("e", 64)
	if e := g.BeforeCreate(ctx, digest, nonce); e != nil {
		t.Fatal(e)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- g.AbortBeforeSend(ctx, digest, nonce) }()
	go func() { <-start; results <- g.AcknowledgePod(ctx, g.Request.RequestID, store.NewID(), nonce) }()
	close(start)
	a, b := <-results, <-results
	if (a == nil) == (b == nil) {
		t.Fatal("ACK and no-send must be exclusive", a, b)
	}
	if f.p.b.f.Count(t, `SELECT count(*) FROM deployment_browser_pods WHERE (not_sent AND pod_uid IS NULL) OR (NOT not_sent AND pod_uid IS NOT NULL)`) != 1 {
		t.Fatal("invalid browser outcome")
	}
}

func TestBrowserNoSendLostCommitAndAtomicFailure(t *testing.T) {
	for _, mode := range []string{"lost-commit", "insert-denied"} {
		t.Run(mode, func(t *testing.T) {
			f, g := unsentBrowserFixture(t)
			ctx := context.Background()
			digest, nonce := strings.Repeat("a", 64), strings.Repeat("e", 64)
			if e := g.BeforeCreate(ctx, digest, nonce); e != nil {
				t.Fatal(e)
			}
			fault := g
			var dropped <-chan struct{}
			if mode == "lost-commit" {
				db, signal := executionCommitFault(t, 1, "deployer")
				dropped = signal
				fault.Journal = store.NewDeploymentJournal(db, func() (releasing.Configuration, error) { return *f.p.cfg, nil })
			} else {
				if _, e := f.p.b.f.Admin.Pool.Exec(`ALTER TABLE deployment_browser_pods ADD CONSTRAINT fixture_deny_abort CHECK (NOT not_sent)`); e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() {
					_, _ = f.p.b.f.Admin.Pool.Exec(`ALTER TABLE deployment_browser_pods DROP CONSTRAINT IF EXISTS fixture_deny_abort`)
				})
			}
			if fault.AbortBeforeSend(ctx, digest, nonce) == nil {
				t.Fatal("unconfirmed abort reported success")
			}
			expected := 0
			if mode == "lost-commit" {
				select {
				case <-dropped:
				default:
					t.Fatal("real COMMIT acknowledgement not dropped")
				}
				expected = 1
			}
			if f.p.b.f.Count(t, `SELECT count(*) FROM deployment_browser_pods WHERE not_sent AND pod_uid IS NULL`) != expected {
				t.Fatal("unexpected durable no-send outcome")
			}
			for _, check := range []func(context.Context) (bool, error){f.j.PendingBrowserCleanup, f.j.UnacknowledgedBrowserCreate} {
				if pending, e := check(ctx); e != nil || pending != (expected == 0) {
					t.Fatal("uncertainty not derived from durable outcome", pending, e)
				}
			}
			if g.BeforeCreate(ctx, digest, nonce) == nil {
				t.Fatal("original CREATE replay authorized")
			}
			if f.p.b.f.Count(t, `SELECT count(*) FROM deployment_browser_evidence`) != 0 || f.p.b.f.Count(t, `SELECT count(*) FROM deployment_health`) != 0 {
				t.Fatal("abort manufactured health")
			}
		})
	}
}
