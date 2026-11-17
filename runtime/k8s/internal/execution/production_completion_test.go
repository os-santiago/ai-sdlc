package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/productioncompletion"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/productionflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"net/http"
	"testing"
	"time"
)

func completedTerminal(t *testing.T) terminalFixture {
	t.Helper()
	f := newTerminalFixture(t)
	for _, step := range []string{"tag", "draft", "publish", "close"} {
		f.effect(t, step)
	}
	if e := f.j.FinishTerminal(context.Background(), f.c); e != nil {
		t.Fatal(e)
	}
	return f
}
func productionLoader(f terminalFixture) productionflow.Loader {
	return func() (authority.Policy, authority.Identity, error) {
		return f.f.p.cfg.Provenance.Build.Outer, f.f.p.b.f.Material.Identities["release-manager"], nil
	}
}
func productionEnvelope(t *testing.T, f terminalFixture) delivery.Envelope {
	t.Helper()
	var raw, signed []byte
	if e := f.j.Pool.QueryRow(`SELECT payload,receipt FROM authorized_outbox WHERE operation='record-production'`).Scan(&raw, &signed); e != nil {
		t.Fatal(e)
	}
	var receipt authority.Receipt
	if authority.DecodeStrict(signed, &receipt) != nil {
		t.Fatal("signature")
	}
	transport, e := f.f.p.b.f.Material.Identities["relay"].Carry(f.f.p.cfg.Provenance.Build.Outer, receipt, http.MethodPost, "/internal/record-production", time.Now())
	if e != nil {
		t.Fatal(e)
	}
	return delivery.Envelope{Receipt: receipt, Transport: transport, Payload: raw}
}
func acceptProduction(t *testing.T, f terminalFixture, envelope delivery.Envelope) (bool, error) {
	t.Helper()
	inbox := delivery.Inbox{DB: f.f.p.b.f.Components["orchestrator"].DB, Target: "orchestrator"}
	_, duplicate, e := inbox.Accept(context.Background(), f.f.p.cfg.Provenance.Build.Outer, envelope, "record-production", http.MethodPost, "/internal/record-production", time.Now(), productionflow.InboxEffect(func() (authority.Policy, error) { return f.f.p.cfg.Provenance.Build.Outer, nil }))
	return duplicate, e
}
func TestProductionCompletionAuthenticatesFullHistoryAndOriginalWork(t *testing.T) {
	for _, ordering := range []string{"intake-before", "intake-after"} {
		t.Run(ordering, func(t *testing.T) {
			f := completedTerminal(t)
			ctx := context.Background()
			r, e := f.j.DB.ReadTerminalForProduction(ctx, f.c.ID)
			if e != nil {
				t.Fatal("producer history", e)
			}
			orchestrator := f.f.p.b.f.Components["orchestrator"].DB
			copy, e := orchestrator.ReadProductionForOrchestrator(ctx, f.c.ID)
			if e != nil || copy.Digest() != r.Digest() {
				t.Fatal("independent history", e)
			}
			if _, e = f.j.DB.ReadProductionForOrchestrator(ctx, f.c.ID); e == nil {
				t.Fatal("wrong-role reader")
			}
			if _, e = orchestrator.Pool.Exec(`UPDATE terminal_requests SET state='quarantined',reason='uncertain',completed_at=NULL`); e == nil {
				t.Fatal("cross-role journal write")
			}
			if ordering == "intake-before" {
				if _, e = f.f.p.b.f.Admin.Pool.Exec(`UPDATE work_items SET status='blocked' WHERE uuid=$1`, r.Source.WorkItemID); e != nil {
					t.Fatal(e)
				}
			}
			nextPlan, nextWork := store.NewID(), store.NewID()
			if e = store.UpsertPlan(ctx, f.f.p.b.f.Admin.Pool, store.PlanRecord{ID: nextPlan, Repository: r.Source.Source.Repository, CorrelationID: store.NewID()}); e != nil {
				t.Fatal(e)
			}
			if e = store.UpsertWorkItem(ctx, f.f.p.b.f.Admin.Pool, store.WorkItemRecord{PlanID: nextPlan, ID: "later-generation", UUID: nextWork, IssueNumbers: []int{r.Source.Source.IssueNumber}, Status: "ready"}); e != nil {
				t.Fatal(e)
			}
			// Historical completion remains recordable when current release target expires.
			// This mutates only the test loader input, not recorded command timestamps.
			f.f.p.cfg.Target.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
			if sent, e := productionflow.DispatchCompleted(ctx, f.j.DB, productionLoader(f)); e != nil || !sent {
				t.Fatal("dispatch", sent, e)
			}
			envelope := productionEnvelope(t, f)
			for i := 0; i < 2; i++ {
				envelope.Transport, e = f.f.p.b.f.Material.Identities["relay"].Carry(f.f.p.cfg.Provenance.Build.Outer, envelope.Receipt, http.MethodPost, "/internal/record-production", time.Now())
				if e != nil {
					t.Fatal(e)
				}
				if duplicate, e := acceptProduction(t, f, envelope); e != nil || duplicate != (i == 1) {
					t.Fatal("inbox", duplicate, e)
				}
			}
			if ordering == "intake-after" {
				if _, e = f.f.p.b.f.Admin.Pool.Exec(`UPDATE work_items SET status='blocked' WHERE uuid=$1 AND status NOT IN('completed','failed')`, r.Source.WorkItemID); e != nil {
					t.Fatal(e)
				}
			}
			var state string
			if e = orchestrator.Pool.QueryRow(`SELECT status FROM work_items WHERE uuid=$1`, r.Source.WorkItemID).Scan(&state); e != nil || state != "completed" {
				t.Fatal("original work not complete", state, e)
			}
			var nextState string
			if e = orchestrator.Pool.QueryRow(`SELECT status FROM work_items WHERE uuid=$1`, nextWork).Scan(&nextState); e != nil || nextState != "ready" {
				t.Fatal("completed a different generation", nextState, e)
			}
			if sent, e := productionflow.DispatchCompleted(ctx, f.j.DB, productionLoader(f)); e != nil || sent {
				t.Fatal("duplicate handoff", e)
			}
			if f.f.p.b.f.Count(t, `SELECT count(*) FROM production_records`) != 1 || f.f.p.b.f.Count(t, `SELECT count(*) FROM production_handoffs`) != 1 {
				t.Fatal("duplicate/missing final fact")
			}
		})
	}
}
func TestProductionCompletionRejectsIncompleteTamperedOrForeignFacts(t *testing.T) {
	for _, mode := range []string{"incomplete", "history", "work-id", "already-completed", "failed", "unsigned"} {
		t.Run(mode, func(t *testing.T) {
			var f terminalFixture
			if mode == "incomplete" {
				f = newTerminalFixture(t)
				if _, e := f.j.DB.ReadTerminalForProduction(context.Background(), f.c.ID); e == nil {
					t.Fatal("incomplete accepted")
				}
				return
			}
			f = completedTerminal(t)
			ctx := context.Background()
			if mode == "history" {
				if _, e := f.f.p.b.f.Admin.Pool.Exec(`UPDATE deployment_health SET digest=repeat('0',64)`); e != nil {
					t.Fatal(e)
				}
				if sent, e := productionflow.DispatchCompleted(ctx, f.j.DB, productionLoader(f)); e == nil || sent {
					t.Fatal("tampered upstream accepted")
				}
				return
			}
			if sent, e := productionflow.DispatchCompleted(ctx, f.j.DB, productionLoader(f)); e != nil || !sent {
				t.Fatal(e)
			}
			envelope := productionEnvelope(t, f)
			if mode == "work-id" {
				var c productioncompletion.Command
				if authority.DecodeStrict(envelope.Payload, &c) != nil {
					t.Fatal("command")
				}
				c.Fact.WorkItemID = store.NewID()
				envelope.Payload, _ = json.Marshal(c)
				p := f.f.p.cfg.Provenance.Build.Outer
				envelope.Receipt, _ = f.f.p.b.f.Material.Identities["release-manager"].Seal(p, c.ID(), "record-production", envelope.Payload)
				envelope.Transport, _ = f.f.p.b.f.Material.Identities["relay"].Carry(p, envelope.Receipt, http.MethodPost, "/internal/record-production", time.Now())
			}
			if mode == "already-completed" || mode == "failed" {
				state := "completed"
				if mode == "failed" {
					state = "failed"
				}
				if _, e := f.f.p.b.f.Admin.Pool.Exec(`UPDATE work_items SET status=$1`, state); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "unsigned" {
				envelope.Transport = authority.Transport{}
			}
			if _, e := acceptProduction(t, f, envelope); e == nil {
				t.Fatal("unbound final fact accepted")
			}
			if f.f.p.b.f.Count(t, `SELECT count(*) FROM production_records`) != 0 {
				t.Fatal("invalid fact survived")
			}
		})
	}
}
func TestProductionCompletionHandoffLostCommitAndAtomicFailure(t *testing.T) {
	for _, mode := range []string{"lost-commit", "outbox-denied"} {
		t.Run(mode, func(t *testing.T) {
			f := completedTerminal(t)
			ctx := context.Background()
			db := f.j.DB
			var dropped <-chan struct{}
			if mode == "lost-commit" {
				db, dropped = executionCommitFault(t, 1, "release-manager")
			} else {
				if _, e := f.f.p.b.f.Admin.Pool.Exec(`ALTER TABLE authorized_outbox ADD CONSTRAINT fixture_deny_production CHECK(operation<>'record-production')`); e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() {
					_, _ = f.f.p.b.f.Admin.Pool.Exec(`ALTER TABLE authorized_outbox DROP CONSTRAINT IF EXISTS fixture_deny_production`)
				})
			}
			if sent, e := productionflow.DispatchCompleted(ctx, db, productionLoader(f)); e == nil || sent {
				t.Fatal("unknown commit accepted")
			}
			expected := 0
			if mode == "lost-commit" {
				select {
				case <-dropped:
				default:
					t.Fatal("actual COMMIT not dropped")
				}
				expected = 1
				if sent, e := productionflow.DispatchCompleted(ctx, f.j.DB, productionLoader(f)); e != nil || sent {
					t.Fatal("redispatch", e)
				}
			}
			if f.f.p.b.f.Count(t, `SELECT count(*) FROM production_handoffs`) != expected || f.f.p.b.f.Count(t, `SELECT count(*) FROM authorized_outbox WHERE operation='record-production'`) != expected {
				t.Fatal("partial handoff")
			}
		})
	}
}

func TestProductionCompletionConsumerLostCommitACKIsIdempotent(t *testing.T) {
	f := completedTerminal(t)
	ctx := context.Background()
	if sent, e := productionflow.DispatchCompleted(ctx, f.j.DB, productionLoader(f)); e != nil || !sent {
		t.Fatal(e)
	}
	envelope := productionEnvelope(t, f)
	db, dropped := executionCommitFault(t, 1, "orchestrator")
	inbox := delivery.Inbox{DB: db, Target: "orchestrator"}
	effect := productionflow.InboxEffect(func() (authority.Policy, error) { return f.f.p.cfg.Provenance.Build.Outer, nil })
	if _, _, e := inbox.Accept(ctx, f.f.p.cfg.Provenance.Build.Outer, envelope, "record-production", http.MethodPost, "/internal/record-production", time.Now(), effect); e == nil {
		t.Fatal("lost consumer COMMIT accepted")
	}
	select {
	case <-dropped:
	default:
		t.Fatal("actual COMMIT not dropped")
	}
	envelope = productionEnvelope(t, f)
	if duplicate, e := acceptProduction(t, f, envelope); e != nil || !duplicate {
		t.Fatal("consumer replay", duplicate, e)
	}
	if f.f.p.b.f.Count(t, `SELECT count(*) FROM production_records`) != 1 || f.f.p.b.f.Count(t, `SELECT count(*) FROM authorized_inbox WHERE target='orchestrator' AND producer='release-manager' AND event_id LIKE 'production-%'`) != 1 || f.f.p.b.f.Count(t, `SELECT count(*) FROM work_items WHERE status='completed'`) != 1 {
		t.Fatal("non-atomic terminal outcome")
	}
}
func waitProductionLock(t *testing.T, f terminalFixture, blocker int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		if e := f.f.p.b.f.Admin.Pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE usename='aisdlc_orchestrator' AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked); e != nil {
			t.Fatal(e)
		}
		if blocked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("actual planner/production SQL wait not observed")
}
func TestProductionCompletionConcurrentPlannerWithdrawalPreservesFact(t *testing.T) {
	for _, order := range []string{"planner-first", "production-first"} {
		t.Run(order, func(t *testing.T) {
			f := completedTerminal(t)
			ctx := context.Background()
			r, e := f.j.DB.ReadTerminalForProduction(ctx, f.c.ID)
			if e != nil {
				t.Fatal(e)
			}
			if sent, e := productionflow.DispatchCompleted(ctx, f.j.DB, productionLoader(f)); e != nil || !sent {
				t.Fatal(e)
			}
			orchestrator := f.f.p.b.f.Components["orchestrator"].DB
			envelope := productionEnvelope(t, f)
			productionDone := make(chan error, 1)
			plannerDone := make(chan error, 1)
			if order == "planner-first" {
				tx, e := orchestrator.Pool.BeginTx(ctx, nil)
				if e != nil {
					t.Fatal(e)
				}
				defer tx.Rollback()
				var pid int
				if e = tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); e != nil {
					t.Fatal(e)
				}
				if e = store.BlockSupersededIssue(ctx, tx, r.Source.Source.Repository, r.Source.Source.IssueNumber); e != nil {
					t.Fatal(e)
				}
				go func() { _, e := acceptProduction(t, f, envelope); productionDone <- e }()
				waitProductionLock(t, f, pid)
				if e = tx.Commit(); e != nil {
					t.Fatal(e)
				}
				if e = <-productionDone; e != nil {
					t.Fatal(e)
				}
			} else {
				lock, e := f.f.p.b.f.Admin.Pool.BeginTx(ctx, nil)
				if e != nil {
					t.Fatal(e)
				}
				defer lock.Rollback()
				if _, e = lock.Exec(`LOCK TABLE production_records IN ACCESS EXCLUSIVE MODE`); e != nil {
					t.Fatal(e)
				}
				var pid int
				if e = lock.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); e != nil {
					t.Fatal(e)
				}
				go func() { _, e := acceptProduction(t, f, envelope); productionDone <- e }()
				waitProductionLock(t, f, pid)
				var productionPID int
				if e = f.f.p.b.f.Admin.Pool.QueryRow(`SELECT pid FROM pg_stat_activity WHERE usename='aisdlc_orchestrator' AND $1=ANY(pg_blocking_pids(pid)) LIMIT 1`, pid).Scan(&productionPID); e != nil {
					t.Fatal(e)
				}
				go func() {
					plannerDone <- orchestrator.WithTx(ctx, func(tx *sql.Tx) error {
						return store.BlockSupersededIssue(ctx, tx, r.Source.Source.Repository, r.Source.Source.IssueNumber)
					})
				}()
				waitProductionLock(t, f, productionPID)
				if e = lock.Commit(); e != nil {
					t.Fatal(e)
				}
				if e = <-productionDone; e != nil {
					t.Fatal(e)
				}
				if e = <-plannerDone; e != nil {
					t.Fatal(e)
				}
			}
			var state string
			if e = orchestrator.Pool.QueryRow(`SELECT status FROM work_items WHERE uuid=$1`, r.Source.WorkItemID).Scan(&state); e != nil || state != "completed" {
				t.Fatal("completion lost in withdrawal race", state, e)
			}
		})
	}
}
