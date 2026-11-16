package execution

import (
	"context"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/completionflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasecompletion"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"testing"
	"time"
)

type terminalFixture struct {
	f           deploymentFixture
	j           *store.TerminalJournal
	c           store.TerminalClaim
	plan        releasecompletion.Plan
	raw, signed []byte
}

func newTerminalFixture(t *testing.T) terminalFixture {
	t.Helper()
	f, _ := completedDeployment(t)
	ctx := context.Background()
	if sent, e := completionflow.DispatchDeployed(ctx, f.j.DB, completionLoader(f)); e != nil || !sent {
		t.Fatal("handoff", e)
	}
	var raw, signed []byte
	if e := f.j.Pool.QueryRow(`SELECT payload,receipt FROM authorized_outbox WHERE operation='record-deployment'`).Scan(&raw, &signed); e != nil {
		t.Fatal(e)
	}
	j := store.NewTerminalJournal(f.p.j.DB, func() (releasing.Configuration, error) { return *f.p.cfg, nil })
	if e := j.QueueTerminal(ctx, raw, signed); e != nil {
		t.Fatal("queue", e)
	}
	var cmd releasecompletion.Command
	if json.Unmarshal(raw, &cmd) != nil {
		t.Fatal("command")
	}
	c, e := j.ClaimTerminal(ctx, cmd.ID())
	if e != nil {
		t.Fatal("claim", e)
	}
	r, e := j.ReadTerminal(ctx, c)
	if e != nil {
		t.Fatal("read", e)
	}
	return terminalFixture{f, j, c, r.Plan, raw, signed}
}
func (f terminalFixture) send(t *testing.T, step string) releasecompletion.EffectRequest {
	t.Helper()
	q, e := f.j.BeginTerminalEffect(context.Background(), f.c, step)
	if e != nil {
		t.Fatal("intent", step, e)
	}
	if e = f.j.SendTerminalEffect(context.Background(), f.c, q); e != nil {
		t.Fatal("send", step, e)
	}
	return q
}
func (f terminalFixture) receipt(q releasecompletion.EffectRequest) releasecompletion.EffectReceipt {
	r := releasecompletion.EffectReceipt{Step: q.Step, PlanSHA256: f.plan.Digest(), RequestSHA256: q.Digest(), Tag: f.plan.Tag, CommitSHA: f.plan.SourceSHA, ObservedAt: time.Now().UnixMilli()}
	if q.Step != "tag" {
		r.ReleaseID = 37
		r.BodySHA256 = authority.Digest([]byte(f.plan.Body))
	}
	if q.Step == "publish" || q.Step == "close" {
		r.ReleaseURL = "https://github.com/" + f.plan.Repository + "/releases/tag/" + f.plan.Tag
	}
	if q.Step == "close" {
		r.IssueNumber = f.plan.IssueNumber
		r.IssueID = 51
	}
	return r
}
func (f terminalFixture) effect(t *testing.T, step string) {
	t.Helper()
	q := f.send(t, step)
	if e := f.j.RecordTerminalReceipt(context.Background(), f.c, f.receipt(q)); e != nil {
		t.Fatal("record", step, e)
	}
}
func TestTerminalJournalOrderedEffectsAndClosureFact(t *testing.T) {
	f := newTerminalFixture(t)
	ctx := context.Background()
	if e := f.j.QueueTerminal(ctx, f.raw, f.signed); e == nil {
		t.Fatal("duplicate command")
	}
	if _, e := f.j.BeginTerminalEffect(ctx, f.c, "publish"); e == nil {
		t.Fatal("publish without own draft")
	}
	for _, step := range []string{"tag", "draft", "publish"} {
		f.effect(t, step)
	}
	q := f.send(t, "close")
	receipt := f.receipt(q)
	// Fixture models intake observing the now closed issue before ACK recording.
	if _, e := f.f.p.b.f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
		t.Fatal(e)
	}
	f.f.p.cfg.Provenance.Build.Outer.Freeze = true
	if e := f.j.RecordTerminalReceipt(ctx, f.c, receipt); e != nil {
		t.Fatal("original closure fact self-revoked", e)
	}
	if e := f.j.FinishTerminal(ctx, f.c); e != nil {
		t.Fatal("complete", e)
	}
	progress, e := f.j.ReadTerminalProgress(ctx, f.c)
	if e != nil || len(progress.Receipts) != 4 || progress.NextStep != "" || progress.ReleaseID != 37 {
		t.Fatal("terminal proof", e)
	}
	if _, e := f.j.BeginTerminalEffect(ctx, f.c, "close"); e == nil {
		t.Fatal("completed effect replay")
	}
	for _, db := range []*store.DB{f.f.j.DB, f.f.p.b.f.Components["orchestrator"].DB} {
		if _, e := db.Pool.Exec(`UPDATE terminal_requests SET state='quarantined',reason='uncertain'`); e == nil {
			t.Fatal("cross-role mutation")
		}
	}
	if _, e := f.j.Pool.Exec(`UPDATE terminal_receipts SET digest=repeat('0',64)`); e == nil {
		t.Fatal("mutable receipts")
	}
}
func TestTerminalJournalRejectsUnacknowledgedAndTamperedEffects(t *testing.T) {
	for _, mode := range []string{"no-send", "wrong-sha", "wrong-request", "wrong-id", "tampered-history", "withdrawn", "replay"} {
		t.Run(mode, func(t *testing.T) {
			f := newTerminalFixture(t)
			ctx := context.Background()
			q, e := f.j.BeginTerminalEffect(ctx, f.c, "tag")
			if e != nil {
				t.Fatal(e)
			}
			if mode == "withdrawn" {
				if _, e := f.f.p.b.f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
					t.Fatal(e)
				}
				if e = f.j.SendTerminalEffect(ctx, f.c, q); e == nil {
					t.Fatal("withdrawn send")
				}
				return
			}
			if mode != "no-send" {
				if e = f.j.SendTerminalEffect(ctx, f.c, q); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "replay" {
				if e = f.j.SendTerminalEffect(ctx, f.c, q); e == nil {
					t.Fatal("send reused")
				}
				p, e := f.j.ReadTerminalProgress(ctx, f.c)
				if e != nil || !p.Uncertain {
					t.Fatal("unknown send not quarantinable", e)
				}
				return
			}
			r := f.receipt(q)
			switch mode {
			case "wrong-sha":
				r.CommitSHA = "main"
			case "wrong-request":
				r.RequestSHA256 = authority.Digest([]byte("foreign"))
			case "wrong-id":
				r.ReleaseID = 99
			case "tampered-history":
				if _, e := f.f.p.b.f.Admin.Pool.Exec(`UPDATE deployment_health SET digest=repeat('0',64)`); e != nil {
					t.Fatal(e)
				}
			}
			if e = f.j.RecordTerminalReceipt(ctx, f.c, r); e == nil {
				t.Fatal("unauthenticated fact")
			}
			if f.f.p.b.f.Count(t, `SELECT count(*) FROM terminal_receipts`) != 0 {
				t.Fatal("invalid receipt durable")
			}
		})
	}
}
func TestTerminalJournalLostCommitACKConservativeRecovery(t *testing.T) {
	for _, stage := range []string{"send", "receipt"} {
		t.Run(stage, func(t *testing.T) {
			f := newTerminalFixture(t)
			ctx := context.Background()
			q, e := f.j.BeginTerminalEffect(ctx, f.c, "tag")
			if e != nil {
				t.Fatal(e)
			}
			db, dropped := executionCommitFault(t, 1, "release-manager")
			fault := store.NewTerminalJournal(db, func() (releasing.Configuration, error) { return *f.f.p.cfg, nil })
			if stage == "send" {
				if e = fault.SendTerminalEffect(ctx, f.c, q); e == nil {
					t.Fatal("lost send COMMIT accepted")
				}
			} else {
				if e = f.j.SendTerminalEffect(ctx, f.c, q); e != nil {
					t.Fatal(e)
				}
				if e = fault.RecordTerminalReceipt(ctx, f.c, f.receipt(q)); e == nil {
					t.Fatal("lost receipt COMMIT accepted")
				}
			}
			select {
			case <-dropped:
			default:
				t.Fatal("actual COMMIT not dropped")
			}
			p, e := f.j.ReadTerminalProgress(ctx, f.c)
			if e != nil {
				t.Fatal(e)
			}
			if stage == "send" {
				if !p.Uncertain || p.NextStep != "tag" {
					t.Fatal("missing uncertain send")
				}
				if e = f.j.SendTerminalEffect(ctx, f.c, q); e == nil {
					t.Fatal("resend")
				}
			} else {
				if p.Uncertain || p.NextStep != "draft" || len(p.Receipts) != 1 {
					t.Fatal("original receipt not recovered")
				}
			}
		})
	}
}

func TestTerminalJournalRejectsCrossStepRelabelledHistory(t *testing.T) {
	f := newTerminalFixture(t)
	ctx := context.Background()
	f.effect(t, "tag")
	var sent time.Time
	if e := f.j.Pool.QueryRow(`SELECT created_at FROM terminal_sends WHERE step='tag'`).Scan(&sent); e != nil {
		t.Fatal(e)
	}
	q, e := releasecompletion.Request(f.plan, "draft", 0, sent)
	if e != nil {
		t.Fatal(e)
	}
	r := f.receipt(q)
	r.ObservedAt = sent.UnixMilli()
	raw, _ := json.Marshal(q)
	receipt, _ := json.Marshal(r)
	if _, e = f.f.p.b.f.Admin.Pool.Exec(`UPDATE terminal_intents SET request=$1,request_digest=$2 WHERE step='tag'`, raw, q.Digest()); e != nil {
		t.Fatal(e)
	}
	if _, e = f.f.p.b.f.Admin.Pool.Exec(`UPDATE terminal_receipts SET receipt=$1,digest=$2 WHERE step='tag'`, receipt, authority.Digest(receipt)); e != nil {
		t.Fatal(e)
	}
	if _, e = f.j.ReadTerminalProgress(ctx, f.c); e == nil {
		t.Fatal("draft history relabelled as tag accepted")
	}
}
func TestTerminalJournalSelectionFiltersChangedIssueAndConfiguration(t *testing.T) {
	f := newTerminalFixture(t)
	ctx := context.Background()
	if _, e := f.f.p.b.f.Admin.Pool.Exec(`UPDATE terminal_requests SET state='queued',claim_token=NULL`); e != nil {
		t.Fatal(e)
	}
	if id, e := f.j.NextTerminal(ctx); e != nil || id != f.c.ID {
		t.Fatal("current selection", id, e)
	}
	original := f.f.p.cfg.Target
	f.f.p.cfg.Target.ExpiresAt--
	if id, e := f.j.NextTerminal(ctx); e != nil || id != "" {
		t.Fatal("changed target selected", id, e)
	}
	f.f.p.cfg.Target = original
	if _, e := f.f.p.b.f.Admin.Pool.Exec(`UPDATE work_items SET status='completed'`); e != nil {
		t.Fatal(e)
	}
	if id, e := f.j.NextTerminal(ctx); e != nil || id != "" {
		t.Fatal("completed work item selected", id, e)
	}
	if _, e := f.f.p.b.f.Admin.Pool.Exec(`UPDATE work_items SET status='running'`); e != nil {
		t.Fatal(e)
	}
	if _, e := f.f.p.b.f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
		t.Fatal(e)
	}
	if id, e := f.j.NextTerminal(ctx); e != nil || id != "" {
		t.Fatal("withdrawn generation selected", id, e)
	}
}
func TestTerminalJournalWithdrawalDuringSendWait(t *testing.T) {
	f := newTerminalFixture(t)
	ctx := context.Background()
	q, e := f.j.BeginTerminalEffect(ctx, f.c, "tag")
	if e != nil {
		t.Fatal(e)
	}
	tx, e := f.f.p.b.f.Admin.Pool.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`LOCK TABLE terminal_sends IN ACCESS EXCLUSIVE MODE`); e != nil {
		t.Fatal(e)
	}
	var blocker int
	if e = tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&blocker); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- f.j.SendTerminalEffect(ctx, f.c, q) }()
	blocked := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e = f.f.p.b.f.Admin.Pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE usename='aisdlc_release_manager' AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked); e != nil {
			t.Fatal(e)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("actual lock wait not observed")
	}
	if _, e = f.f.p.b.f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e == nil {
		t.Fatal("withdrawn send allowed after wait")
	}
	if f.f.p.b.f.Count(t, `SELECT count(*) FROM terminal_sends`) != 0 {
		t.Fatal("withdrawn fence survived")
	}
}
func TestTerminalJournalFinishLostCommitACKPreservesCompletedFact(t *testing.T) {
	f := newTerminalFixture(t)
	ctx := context.Background()
	for _, step := range []string{"tag", "draft", "publish", "close"} {
		f.effect(t, step)
	}
	db, dropped := executionCommitFault(t, 1, "release-manager")
	j := store.NewTerminalJournal(db, func() (releasing.Configuration, error) { return *f.f.p.cfg, nil })
	if e := j.FinishTerminal(ctx, f.c); e == nil {
		t.Fatal("lost COMMIT ACK accepted")
	}
	select {
	case <-dropped:
	default:
		t.Fatal("COMMIT not dropped")
	}
	if c, e := f.j.NextTerminalRecovery(ctx); e != nil || c.ID != "" {
		t.Fatal("completed fact requeued", e)
	}
	p, e := f.j.ReadTerminalProgress(ctx, f.c)
	if e != nil || len(p.Receipts) != 4 || p.NextStep != "" {
		t.Fatal("completed fact lost", e)
	}
}
