package execution

import (
	"context"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type promotionFixture struct {
	b                buildFixture
	cfg              *releasing.Configuration
	j                *store.PromotionJournal
	record           store.ProvenanceRecord
	built            store.BuiltRecord
	command          releasing.Command
	payload, receipt []byte
	plan             releasing.Plan
}

func newPromotionFixture(t *testing.T) promotionFixture {
	t.Helper()
	ctx := context.Background()
	p := newProvenanceFixture(t)
	claim := p.claim(t)
	if e := p.j.FinishProvenance(ctx, claim, p.proof); e != nil {
		t.Fatal(e)
	}
	record, e := p.j.ReadProvenanceRecord(ctx, claim.ID)
	if e != nil {
		t.Fatal(e)
	}
	target := releasing.Target{Version: releasing.TargetVersion, Scope: "kind-test-production", Repository: p.cfg.Trust.Repository, Environment: p.cfg.Trust.Environment, ClusterUID: p.cfg.Trust.ClusterUID, NamespaceUID: store.NewID(), Namespace: p.cfg.Trust.Environment, Deployment: "saferus-console", DeploymentUID: store.NewID(), Container: "console", DeploymentSpecSHA256: strings.Repeat("5", 64), Service: "saferus-console", ServiceUID: store.NewID(), ServiceSpecSHA256: strings.Repeat("6", 64), PullSecret: "ai-sdlc-release-reader", PullSecretUID: store.NewID(), TLSSecretUID: store.NewID(), TLSCA: p.cfg.Trust.RegistryCA, TLSHostname: "saferus-console", BaselineImage: "localhost/saferus-console@sha256:" + strings.Repeat("7", 64), BaselineRuntimeDigest: "sha256:" + strings.Repeat("7", 64), RolloutSeconds: 300, SmokeSeconds: 30, ApprovedAt: time.Now().Add(-time.Minute).UnixMilli(), ExpiresAt: time.Now().Add(30 * time.Minute).UnixMilli()}
	cfg := &releasing.Configuration{Provenance: *p.cfg, Target: target}
	c, e := releasing.NewCommand(*cfg, record, p.record, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	payload, _ := json.Marshal(c)
	signed, e := p.b.f.Material.Identities["verifier"].Seal(cfg.Provenance.Build.Outer, c.ID(), "promote-artifact", payload)
	if e != nil {
		t.Fatal(e)
	}
	receipt, _ := json.Marshal(signed)
	if e = p.b.f.Admin.ProvisionRolesForPromotion(ctx, p.b.passwords, "verification", true, true, true, true, true, true); e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	u.User = url.UserPassword("aisdlc_release_manager", p.b.passwords["release-manager"])
	db, e := store.Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = db.Close() })
	plan, e := releasing.CompilePlan(*cfg, c, record, p.record, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	return promotionFixture{p.b, cfg, store.NewPromotionJournal(db, func() (releasing.Configuration, error) { return *cfg, nil }), record, p.record, c, payload, receipt, plan}
}
func (p promotionFixture) claim(t *testing.T) store.PromotionClaim {
	t.Helper()
	ctx := context.Background()
	if e := p.j.QueuePromotion(ctx, p.payload, p.receipt); e != nil {
		t.Fatal("queue", e)
	}
	c, e := p.j.ClaimPromotion(ctx, p.command.ID())
	if e != nil {
		t.Fatal("claim", e)
	}
	return c
}
func TestPromotionJournalExclusiveClaimAndImmutableCompletion(t *testing.T) {
	p := newPromotionFixture(t)
	ctx := context.Background()
	if e := p.j.QueuePromotion(ctx, p.payload, p.receipt); e != nil {
		t.Fatal(e)
	}
	if p.j.QueuePromotion(ctx, p.payload, p.receipt) == nil {
		t.Fatal("duplicate queue")
	}
	var wg sync.WaitGroup
	claims := make(chan store.PromotionClaim, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, e := p.j.ClaimPromotion(ctx, p.command.ID()); e == nil {
				claims <- c
			}
		}()
	}
	wg.Wait()
	close(claims)
	var winner store.PromotionClaim
	n := 0
	for c := range claims {
		n++
		winner = c
	}
	if n != 1 {
		t.Fatalf("claims=%d", n)
	}
	cfg, r, e := p.j.ReadPromotionInput(ctx, winner)
	if e != nil || cfg.Provenance.Trust.Digest() != p.cfg.Provenance.Trust.Digest() || r.Provenance.Digest() != p.record.Digest() {
		t.Fatal("input", e)
	}
	if e = p.j.FinishPromotion(ctx, winner, p.plan); e != nil {
		t.Fatal("finish", e)
	}
	record, e := p.j.ReadPromotionRecord(ctx, winner.ID)
	if e != nil || record.Plan != p.plan {
		t.Fatal("historical record", e)
	}
	for _, q := range []string{`UPDATE built_records SET result=result`, `DELETE FROM provenance_records`, `UPDATE provenance_requests SET state='failed',reason='rejected'`, `UPDATE build_contexts SET context=context`} {
		if _, e = p.j.Pool.Exec(q); e == nil {
			t.Fatal("promotion reader can mutate upstream history", q)
		}
	}
	p.cfg.Provenance.Build.Outer.Freeze = true
	again, e := p.j.ReadPromotionRecord(ctx, winner.ID)
	if e != nil || again.Digest() != record.Digest() {
		t.Fatal("history changed with current policy", e)
	}
	if p.j.FinishPromotion(ctx, winner, p.plan) == nil {
		t.Fatal("completion replay")
	}
	for _, q := range []string{`UPDATE promotion_records SET plan=plan`, `DELETE FROM promotion_records`, `UPDATE promotion_requests SET payload=payload`, `UPDATE promotion_requests SET configuration=configuration`, `UPDATE promotion_requests SET expires_at=clock_timestamp()`} {
		if _, e = p.j.Pool.Exec(q); e == nil {
			t.Fatal("immutable authority mutation", q)
		}
	}
	for _, db := range []*store.DB{p.b.journal.DB, p.b.f.db, p.b.f.Admin} {
		j := store.NewPromotionJournal(db, func() (releasing.Configuration, error) { return *p.cfg, nil })
		if _, e = j.ReadPromotionRecord(ctx, winner.ID); e == nil {
			t.Fatal("wrong role read result")
		}
	}
	if e = p.b.f.Admin.ProvisionRolesForProvenance(ctx, p.b.passwords, "verification", true, true, true, true, true); e != nil {
		t.Fatal(e)
	}
	if _, e = p.j.ReadPromotionRecord(ctx, winner.ID); e == nil {
		t.Fatal("provenance capability retained promotion access")
	}
}
func TestPromotionJournalCurrentAuthorityAndAtomicRollback(t *testing.T) {
	for _, name := range []string{"freeze", "trust", "revoked-key", "withdrawn", "bad-plan", "insert-failure", "clock"} {
		t.Run(name, func(t *testing.T) {
			p := newPromotionFixture(t)
			ctx := context.Background()
			c := p.claim(t)
			plan := p.plan
			switch name {
			case "freeze":
				p.cfg.Provenance.Build.Inner.Freeze = true
			case "trust":
				p.cfg.Provenance.Trust.ExpiresAt--
			case "revoked-key":
				delete(p.cfg.Provenance.Build.Outer.Keys, p.b.f.Material.Identities["verifier"].KeyID)
			case "withdrawn":
				if _, e := p.b.f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
					t.Fatal(e)
				}
			case "bad-plan":
				plan.Image = "attacker.invalid/x:latest"
			case "insert-failure":
				if _, e := p.b.f.Admin.Pool.Exec(`REVOKE INSERT(request_id,digest,plan) ON promotion_records FROM aisdlc_release_manager`); e != nil {
					t.Fatal(e)
				}
			case "clock":
				c.Until = c.Until.Add(time.Millisecond)
			}
			if p.j.FinishPromotion(ctx, c, plan) == nil {
				t.Fatal("stale or invalid completion")
			}
			if p.b.f.Count(t, `SELECT count(*) FROM promotion_records`) != 0 || p.b.f.Count(t, `SELECT count(*) FROM promotion_requests WHERE state='planned'`) != 0 {
				t.Fatal("partial completion")
			}
		})
	}
}
func TestPromotionJournalLostAcknowledgementAndRestart(t *testing.T) {
	for _, phase := range []string{"claim", "finish"} {
		t.Run(phase, func(t *testing.T) {
			p := newPromotionFixture(t)
			ctx := context.Background()
			if e := p.j.QueuePromotion(ctx, p.payload, p.receipt); e != nil {
				t.Fatal(e)
			}
			var c store.PromotionClaim
			var e error
			if phase == "finish" {
				c, e = p.j.ClaimPromotion(ctx, p.command.ID())
				if e != nil {
					t.Fatal(e)
				}
			}
			db, dropped := executionCommitFault(t, 1, "release-manager")
			j := store.NewPromotionJournal(db, func() (releasing.Configuration, error) { return *p.cfg, nil })
			if phase == "claim" {
				returned, e := j.ClaimPromotion(ctx, p.command.ID())
				if e == nil || returned.Token != "" {
					t.Fatal("uncertain claim exposed")
				}
			} else if j.FinishPromotion(ctx, c, p.plan) == nil {
				t.Fatal("lost finish acknowledgement")
			}
			select {
			case <-dropped:
			default:
				t.Fatal("COMMIT response not dropped")
			}
			restarted := store.NewPromotionJournal(p.j.DB, func() (releasing.Configuration, error) { return *p.cfg, nil })
			if _, e = restarted.ClaimPromotion(ctx, p.command.ID()); e == nil {
				t.Fatal("restart replay")
			}
			if phase == "finish" {
				r, e := restarted.ReadPromotionRecord(ctx, p.command.ID())
				if e != nil || r.Plan != p.plan {
					t.Fatal("durable plan lost", e)
				}
			}
		})
	}
}

func TestPromotionJournalRealExpiryDoesNotRenewHistory(t *testing.T) {
	p := newPromotionFixture(t)
	ctx := context.Background()
	// Preserve the real 10-second claim budget, then let wall-clock authority
	// expire. No test SQL rewrites timestamps or issues a replacement command.
	p.command.ExpiresAt = time.Now().Add(12 * time.Second).UnixMilli()
	p.payload, _ = json.Marshal(p.command)
	signed, e := p.b.f.Material.Identities["verifier"].Seal(p.cfg.Provenance.Build.Outer, p.command.ID(), "promote-artifact", p.payload)
	if e != nil {
		t.Fatal(e)
	}
	p.receipt, _ = json.Marshal(signed)
	p.plan, e = releasing.CompilePlan(*p.cfg, p.command, p.record, p.built, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	c := p.claim(t)
	time.Sleep(time.Until(c.Until.Add(5 * time.Millisecond)))
	if _, _, e = p.j.ReadPromotionInput(ctx, c); e == nil {
		t.Fatal("expired authority read input")
	}
	if p.j.FinishPromotion(ctx, c, p.plan) == nil {
		t.Fatal("expired authority completed")
	}
	if e = p.j.ExpirePromotion(ctx); e != nil {
		t.Fatal(e)
	}
	if p.b.f.Count(t, `SELECT count(*) FROM promotion_requests WHERE state='failed' AND reason='expired'`) != 1 || p.b.f.Count(t, `SELECT count(*) FROM promotion_records`) != 0 {
		t.Fatal("expiration state")
	}
	if _, e = p.j.ClaimPromotion(ctx, c.ID); e == nil {
		t.Fatal("expired claim renewed")
	}
	// A differently timed signed command still cannot reuse the same build.
	now := time.Now()
	next, e := releasing.NewCommand(*p.cfg, p.record, p.built, now)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(next)
	s, e := p.b.f.Material.Identities["verifier"].Seal(p.cfg.Provenance.Build.Outer, next.ID(), "promote-artifact", raw)
	if e != nil {
		t.Fatal(e)
	}
	receipt, _ := json.Marshal(s)
	if p.j.QueuePromotion(ctx, raw, receipt) == nil {
		t.Fatal("another command renewed consumed build")
	}
}

func TestPromotionJournalRevocationDuringBlockedHistory(t *testing.T) {
	for _, phase := range []string{"queue", "claim", "input", "finish"} {
		t.Run(phase, func(t *testing.T) {
			p := newPromotionFixture(t)
			ctx := context.Background()
			var claim store.PromotionClaim
			if phase == "input" || phase == "finish" {
				claim = p.claim(t)
			} else if phase == "claim" {
				if e := p.j.QueuePromotion(ctx, p.payload, p.receipt); e != nil {
					t.Fatal(e)
				}
			}
			// Synchronize configuration snapshots without racing on shared policy maps.
			var mu sync.Mutex
			j := store.NewPromotionJournal(p.j.DB, func() (releasing.Configuration, error) { mu.Lock(); defer mu.Unlock(); return *p.cfg, nil })
			tx, e := p.b.f.Admin.Pool.BeginTx(ctx, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback()
			if _, e = tx.Exec(`LOCK TABLE build_contexts IN ACCESS EXCLUSIVE MODE`); e != nil {
				t.Fatal(e)
			}
			var blocker int
			if e = tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&blocker); e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() {
				var e error
				switch phase {
				case "queue":
					e = j.QueuePromotion(ctx, p.payload, p.receipt)
				case "claim":
					var c store.PromotionClaim
					c, e = j.ClaimPromotion(ctx, p.command.ID())
					if c.Token != "" {
						e = nil
					}
				case "input":
					var cfg releasing.Configuration
					var r store.PromotionInput
					cfg, r, e = j.ReadPromotionInput(ctx, claim)
					if cfg.Provenance.Trust.Version != "" || r.Command.Version != "" {
						e = nil
					}
				case "finish":
					e = j.FinishPromotion(ctx, claim, p.plan)
				}
				done <- e
			}()
			blocked := false
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if e = p.b.f.Admin.Pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE usename='aisdlc_release_manager' AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked); e != nil {
					t.Fatal(e)
				}
				if blocked {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !blocked {
				t.Fatal("release manager did not observably block on historical context")
			}
			mu.Lock()
			p.cfg.Provenance.Build.Outer.Freeze = true
			mu.Unlock()
			if e = tx.Commit(); e != nil {
				t.Fatal(e)
			}
			if e = <-done; e == nil {
				t.Fatal("revocation during history read exposed authority or succeeded")
			}
			if p.b.f.Count(t, `SELECT count(*) FROM promotion_records`) != 0 {
				t.Fatal("revoked completion persisted")
			}
			if phase == "claim" && p.b.f.Count(t, `SELECT count(*) FROM promotion_requests WHERE state='claimed'`) != 0 {
				t.Fatal("revoked claim committed")
			}
			if phase == "queue" && p.b.f.Count(t, `SELECT count(*) FROM promotion_requests`) != 0 {
				t.Fatal("revoked queue committed")
			}
		})
	}
}
