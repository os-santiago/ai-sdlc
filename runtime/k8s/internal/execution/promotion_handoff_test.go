package execution

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releaseflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

func promotionVerifier(t *testing.T, p promotionFixture) *store.DB {
	t.Helper()
	u, e := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	u.User = url.UserPassword("aisdlc_verifier", p.b.passwords["verifier"])
	db, e := store.Open(context.Background(), u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
func TestPromotionSignedHandoffInboxAndRuntime(t *testing.T) {
	p := newPromotionFixture(t)
	db := promotionVerifier(t, p)
	ctx := context.Background()
	load := func() (releasing.Configuration, authority.Identity, error) {
		return *p.cfg, p.b.f.Material.Identities["verifier"], nil
	}
	if sent, e := releaseflow.DispatchVerified(ctx, db, load); e != nil || !sent {
		t.Fatal("dispatch", e)
	}
	if sent, e := releaseflow.DispatchVerified(ctx, db, load); e != nil || sent {
		t.Fatal("dispatch replay", e)
	}
	var raw, payload []byte
	if e := db.Pool.QueryRow(`SELECT payload,receipt FROM authorized_outbox WHERE operation='promote-artifact'`).Scan(&payload, &raw); e != nil {
		t.Fatal(e)
	}
	var signed authority.Receipt
	if authority.DecodeStrict(raw, &signed) != nil {
		t.Fatal("outbox")
	}
	c, e := releasing.VerifyCommand(*p.cfg, signed, payload, time.Now())
	if e != nil || c.ProvenanceRecordSHA256 != p.record.Digest() {
		t.Fatal("command", e)
	}
	envelope := delivery.Envelope{Receipt: signed, Payload: payload}
	inbox := delivery.Inbox{DB: p.j.DB, Target: "release-manager"}
	if _, _, e = inbox.Accept(ctx, p.cfg.Provenance.Build.Outer, envelope, "promote-artifact", http.MethodPost, "/internal/promote-artifact", time.Now(), releaseflow.InboxEffect(p.j)); e == nil {
		t.Fatal("untrusted transport")
	}
	for n := 0; n < 2; n++ {
		envelope.Transport, e = p.b.f.Material.Identities["relay"].Carry(p.cfg.Provenance.Build.Outer, signed, http.MethodPost, "/internal/promote-artifact", time.Now())
		if e != nil {
			t.Fatal(e)
		}
		_, dup, e := inbox.Accept(ctx, p.cfg.Provenance.Build.Outer, envelope, "promote-artifact", http.MethodPost, "/internal/promote-artifact", time.Now(), releaseflow.InboxEffect(p.j))
		if e != nil || dup != (n == 1) {
			t.Fatal("inbox", dup, e)
		}
	}
	if p.b.f.Count(t, `SELECT count(*) FROM promotion_requests`) != 1 || p.b.f.Count(t, `SELECT count(*) FROM promotion_handoffs`) != 1 {
		t.Fatal("duplicate effects")
	}
	runtime := releaseflow.Runtime{DB: p.j.DB, Load: func() (releasing.Configuration, error) { return *p.cfg, nil }}
	if state, e := runtime.PollOnce(ctx); e != nil || state != "promotion-recorded" {
		t.Fatal("runtime", state, e)
	}
	if state, e := runtime.PollOnce(ctx); e != nil || state != "promotion-idle" {
		t.Fatal("runtime replay", state, e)
	}
	record, e := p.j.ReadPromotionRecord(ctx, c.ID())
	if e != nil || record.Plan.ProvenanceRecordSHA256 != p.record.Digest() {
		t.Fatal("plan", e)
	}
	// This test has no Kubernetes/GitHub adapter. Durable plan is the terminal
	// result of this inactive increment, not proof of a deployed application.
}
func TestPromotionHandoffFailureIsAtomic(t *testing.T) {
	for _, name := range []string{"outbox-failure", "changed-policy", "changed-target", "wrong-role", "lost-commit"} {
		t.Run(name, func(t *testing.T) {
			p := newPromotionFixture(t)
			db := promotionVerifier(t, p)
			ctx := context.Background()
			n := 0
			load := func() (releasing.Configuration, authority.Identity, error) {
				n++
				if n == 2 {
					if name == "changed-policy" {
						p.cfg.Provenance.Build.Outer.Freeze = true
					}
					if name == "changed-target" {
						p.cfg.Target.DeploymentUID = store.NewID()
					}
				}
				return *p.cfg, p.b.f.Material.Identities["verifier"], nil
			}
			if name == "outbox-failure" {
				if _, e := p.b.f.Admin.Pool.Exec(`ALTER TABLE authorized_outbox ADD CONSTRAINT fixture_deny_promotion CHECK(operation<>'promote-artifact')`); e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() {
					if _, e := p.b.f.Admin.Pool.Exec(`ALTER TABLE authorized_outbox DROP CONSTRAINT fixture_deny_promotion`); e != nil {
						t.Error(e)
					}
				})
			}
			if name == "wrong-role" {
				db = p.j.DB
			}
			var dropped <-chan struct{}
			if name == "lost-commit" {
				db, dropped = executionCommitFault(t, 1, "verifier")
			}
			if sent, e := releaseflow.DispatchVerified(ctx, db, load); e == nil || sent {
				t.Fatal("unsafe/ambiguous handoff", e)
			}
			expected := 0
			if name == "lost-commit" {
				expected = 1
				select {
				case <-dropped:
				default:
					t.Fatal("commit ACK not dropped")
				}
				db = promotionVerifier(t, p)
				if sent, e := releaseflow.DispatchVerified(ctx, db, load); e != nil || sent {
					t.Fatal("restart repeated handoff", e)
				}
			}
			if p.b.f.Count(t, `SELECT count(*) FROM promotion_handoffs`) != expected || p.b.f.Count(t, `SELECT count(*) FROM authorized_outbox WHERE operation='promote-artifact'`) != expected {
				t.Fatal("partial or repeated dispatch")
			}
		})
	}
}
func TestPromotionRuntimeResolvesOnlyRecordedLostCompletion(t *testing.T) {
	p := newPromotionFixture(t)
	ctx := context.Background()
	if e := p.j.QueuePromotion(ctx, p.payload, p.receipt); e != nil {
		t.Fatal(e)
	}
	db, dropped := executionCommitFault(t, 4, "release-manager")
	runtime := releaseflow.Runtime{DB: db, Load: func() (releasing.Configuration, error) { return *p.cfg, nil }}
	if state, e := runtime.PollOnce(ctx); e != nil || state != "promotion-recorded" {
		t.Fatal("lost completion", state, e)
	}
	select {
	case <-dropped:
	default:
		t.Fatal("completion ACK not dropped")
	}
	if p.b.f.Count(t, `SELECT count(*) FROM promotion_records`) != 1 {
		t.Fatal("durable decision")
	}
	if state, e := runtime.PollOnce(ctx); e != nil || state != "promotion-idle" {
		t.Fatal("replayed decision", state, e)
	}
}
func TestPromotionSelectorsRejectWithdrawnIssue(t *testing.T) {
	p := newPromotionFixture(t)
	db := promotionVerifier(t, p)
	ctx := context.Background()
	if id, e := db.NextVerifiedForPromotion(ctx, *p.cfg); e != nil || id != p.record.Command.ID() {
		t.Fatal("positive history", id, e)
	}
	if e := p.j.QueuePromotion(ctx, p.payload, p.receipt); e != nil {
		t.Fatal(e)
	}
	if id, e := p.j.NextPromotion(ctx); e != nil || id != p.command.ID() {
		t.Fatal("positive queue", id, e)
	}
	if _, e := p.b.f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
		t.Fatal(e)
	}
	if id, e := db.NextVerifiedForPromotion(ctx, *p.cfg); e != nil || id != "" {
		t.Fatal("withdrawn history", id, e)
	}
	if id, e := p.j.NextPromotion(ctx); e != nil || id != "" {
		t.Fatal("withdrawn queue", id, e)
	}
}
func TestPromotionHandoffWithdrawalDuringOutboxWait(t *testing.T) {
	p := newPromotionFixture(t)
	ctx := context.Background()
	db := promotionVerifier(t, p)
	tx, e := p.b.f.Admin.Pool.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`LOCK TABLE authorized_outbox IN ACCESS EXCLUSIVE MODE`); e != nil {
		t.Fatal(e)
	}
	var blocker int
	if e = tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&blocker); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() {
		_, e := releaseflow.DispatchVerified(ctx, db, func() (releasing.Configuration, authority.Identity, error) {
			return *p.cfg, p.b.f.Material.Identities["verifier"], nil
		})
		done <- e
	}()
	blocked := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e = p.b.f.Admin.Pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE usename='aisdlc_verifier' AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked); e != nil {
			t.Fatal(e)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("outbox wait was not observed")
	}
	if _, e = p.b.f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e == nil {
		t.Fatal("withdrawal during outbox wait dispatched stale command")
	}
	if p.b.f.Count(t, `SELECT count(*) FROM promotion_handoffs`) != 0 || p.b.f.Count(t, `SELECT count(*) FROM authorized_outbox WHERE operation='promote-artifact'`) != 0 {
		t.Fatal("stale partial handoff")
	}
}
