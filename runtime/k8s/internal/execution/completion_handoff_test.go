package execution

import (
	"context"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/completionflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasecompletion"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"strings"
	"testing"
	"time"
)

func completedDeployment(t *testing.T) (deploymentFixture, store.DeploymentClaim) {
	t.Helper()
	f := newDeploymentFixture(t)
	c := f.claim(t)
	ctx := context.Background()
	r := f.effect(t, c, f.before(), false)
	if e := f.j.RecordDeploymentReceipt(ctx, c, r); e != nil {
		t.Fatal(e)
	}
	if e := f.j.FinishDeployment(ctx, c, f.attestedHealth(t, c, r), false); e != nil {
		t.Fatal(e)
	}
	return f, c
}
func completionLoader(f deploymentFixture) completionflow.DispatchLoader {
	return func() (releasing.Configuration, authority.Identity, error) {
		return *f.p.cfg, f.p.b.f.Material.Identities["deployer"], nil
	}
}
func TestCompletionAuthenticatedHistoryAndSignedHandoff(t *testing.T) {
	f, c := completedDeployment(t)
	ctx := context.Background()
	original, e := f.j.ReadDeploymentRecord(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	read, e := f.p.j.DB.ReadDeploymentForRelease(ctx, c.ID)
	if e != nil || read.Deployment.Digest() != original.Digest() {
		t.Fatal("authenticated release reader", e)
	}
	if _, e = f.j.DB.ReadDeploymentForRelease(ctx, c.ID); e == nil {
		t.Fatal("deployer used release-manager reader")
	}
	if _, e = f.p.j.DB.ReadDeployedForCompletion(ctx, c.ID); e == nil {
		t.Fatal("release-manager used deployer dispatcher reader")
	}
	if _, e = f.p.j.Pool.Exec(`UPDATE deployment_requests SET state='quarantined' WHERE id=$1`, c.ID); e == nil {
		t.Fatal("release manager mutated deployer journal")
	}
	sent, e := completionflow.DispatchDeployed(ctx, f.j.DB, completionLoader(f))
	if e != nil || !sent {
		t.Fatal("handoff", sent, e)
	}
	if sent, e = completionflow.DispatchDeployed(ctx, f.j.DB, completionLoader(f)); e != nil || sent {
		t.Fatal("duplicate handoff", sent, e)
	}
	var payload, receipt []byte
	if e = f.j.Pool.QueryRow(`SELECT payload,receipt FROM authorized_outbox WHERE operation='record-deployment'`).Scan(&payload, &receipt); e != nil {
		t.Fatal(e)
	}
	var signed authority.Receipt
	if authority.DecodeStrict(receipt, &signed) != nil {
		t.Fatal("invalid signed handoff")
	}
	cmd, e := releasecompletion.VerifyCommand(*f.p.cfg, signed, payload, time.Now())
	if e != nil || cmd.DeploymentRecordSHA256 != original.Digest() {
		t.Fatal("command mismatch", e)
	}
	if cmd.BindRecords(*f.p.cfg, read.Deployment, read.Upstream.Promotion, read.Upstream.Provenance, read.Upstream.Build, time.Now()) != nil {
		t.Fatal("authenticated chain mismatch")
	}
	if f.p.b.f.Count(t, `SELECT count(*) FROM completion_handoffs`) != 1 {
		t.Fatal("missing unique handoff")
	}
	alias := []byte(strings.Replace(string(payload), `"version":`, `"Version":`, 1))
	aliasSigned, e := f.p.b.f.Material.Identities["deployer"].Seal(f.p.cfg.Provenance.Build.Outer, cmd.ID(), "record-deployment", alias)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = releasecompletion.VerifyCommand(*f.p.cfg, aliasSigned, alias, time.Now()); e == nil {
		t.Fatal("signed noncanonical JSON key accepted")
	}
	// Signature, payload and policy remain separate fences.
	altered := cmd
	altered.DeploymentRecordSHA256 = authority.Digest([]byte("foreign"))
	raw, _ := json.Marshal(altered)
	if _, e = releasecompletion.VerifyCommand(*f.p.cfg, signed, raw, time.Now()); e == nil {
		t.Fatal("forged completion")
	}
	cfg := *f.p.cfg
	cfg.Provenance.Build.Outer.Freeze = true
	if _, e = releasecompletion.VerifyCommand(cfg, signed, payload, time.Now()); e == nil {
		t.Fatal("frozen completion accepted")
	}
}
func TestCompletionRejectsTamperedHistoryAndWithdrawal(t *testing.T) {
	for _, mode := range []string{"health", "receipt", "upstream", "withdrawn", "outbox-failure"} {
		t.Run(mode, func(t *testing.T) {
			f, c := completedDeployment(t)
			ctx := context.Background()
			switch mode {
			case "health":
				if _, e := f.p.b.f.Admin.Pool.Exec(`UPDATE deployment_health SET digest=repeat('0',64)`); e != nil {
					t.Fatal(e)
				}
			case "receipt":
				if _, e := f.p.b.f.Admin.Pool.Exec(`UPDATE deployment_receipts SET digest=repeat('0',64)`); e != nil {
					t.Fatal(e)
				}
			case "upstream":
				if _, e := f.p.b.f.Admin.Pool.Exec(`UPDATE promotion_records SET digest=repeat('0',64)`); e != nil {
					t.Fatal(e)
				}
			case "withdrawn":
				if _, e := f.p.b.f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
					t.Fatal(e)
				}
			case "outbox-failure":
				if _, e := f.p.b.f.Admin.Pool.Exec(`ALTER TABLE authorized_outbox ADD CONSTRAINT fixture_deny_completion CHECK(operation<>'record-deployment')`); e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() {
					_, _ = f.p.b.f.Admin.Pool.Exec(`ALTER TABLE authorized_outbox DROP CONSTRAINT IF EXISTS fixture_deny_completion`)
				})
			}
			if mode == "health" || mode == "receipt" || mode == "upstream" {
				if _, e := f.p.j.DB.ReadDeploymentForRelease(ctx, c.ID); e == nil {
					t.Fatal("tampered historical evidence accepted")
				}
			}
			if sent, e := completionflow.DispatchDeployed(ctx, f.j.DB, completionLoader(f)); sent || mode != "withdrawn" && e == nil {
				t.Fatal("invalid completion dispatched", sent, e)
			}
			if f.p.b.f.Count(t, `SELECT count(*) FROM completion_handoffs`) != 0 || f.p.b.f.Count(t, `SELECT count(*) FROM authorized_outbox WHERE operation='record-deployment'`) != 0 {
				t.Fatal("partial completion survived")
			}
		})
	}
}
func TestCompletionNeverTreatsRollbackAsRelease(t *testing.T) {
	f := newDeploymentFixture(t)
	c := f.claim(t)
	ctx := context.Background()
	r := f.effect(t, c, f.before(), false)
	if e := f.j.RecordDeploymentReceipt(ctx, c, r); e != nil {
		t.Fatal(e)
	}
	before := r.After
	before.ObservedAt = time.Now().UnixMilli()
	rollback := f.effect(t, c, before, true)
	if e := f.j.RecordDeploymentReceipt(ctx, c, rollback); e != nil {
		t.Fatal(e)
	}
	if e := f.j.FinishDeployment(ctx, c, f.attestedHealth(t, c, rollback), true); e != nil {
		t.Fatal(e)
	}
	if _, e := f.p.j.DB.ReadDeploymentForRelease(ctx, c.ID); e == nil {
		t.Fatal("rollback accepted as successful release")
	}
	if sent, e := completionflow.DispatchDeployed(ctx, f.j.DB, completionLoader(f)); e != nil || sent {
		t.Fatal("rollback dispatch", sent, e)
	}
}

func TestCompletionWindowDoesNotRenewExpiredDeployment(t *testing.T) {
	f, c := completedDeployment(t)
	r, e := f.j.DB.ReadDeployedForCompletion(context.Background(), c.ID)
	if e != nil {
		t.Fatal(e)
	}
	// Pure command-clock boundary over authenticated fixture history; this does
	// not rewrite database time or claim that a live deployment was resumed.
	now := time.UnixMilli(r.Deployment.Command.ExpiresAt).Add(time.Millisecond)
	cmd, e := releasecompletion.NewCommand(*f.p.cfg, r.Deployment, r.Upstream.Promotion, r.Upstream.Provenance, r.Upstream.Build, now)
	if e != nil {
		t.Fatal("historical success could not authorize separate recording", e)
	}
	if r.Deployment.Command.ExpiresAt != c.Until.UnixMilli() || r.Deployment.Command.BindRecords(*f.p.cfg, r.Upstream.Promotion, r.Upstream.Provenance, r.Upstream.Build, now) == nil {
		t.Fatal("expired deployment authority renewed")
	}
	if cmd.ExpiresAt > now.Add(releasecompletion.MaxAge).UnixMilli() || cmd.ExpiresAt > f.p.cfg.Target.ExpiresAt || cmd.ExpiresAt > f.p.cfg.Provenance.Trust.ExpiresAt {
		t.Fatal("terminal window exceeds original approvals")
	}
}
func TestCompletionLostCommitAcknowledgementDoesNotRedispatch(t *testing.T) {
	f, _ := completedDeployment(t)
	ctx := context.Background()
	db, dropped := executionCommitFault(t, 1, "deployer")
	if sent, e := completionflow.DispatchDeployed(ctx, db, completionLoader(f)); e == nil || sent {
		t.Fatal("lost commit acknowledgement accepted")
	}
	select {
	case <-dropped:
	default:
		t.Fatal("actual COMMIT response was not dropped")
	}
	if sent, e := completionflow.DispatchDeployed(ctx, f.j.DB, completionLoader(f)); e != nil || sent {
		t.Fatal("restart replayed original completion", sent, e)
	}
	if f.p.b.f.Count(t, `SELECT count(*) FROM completion_handoffs`) != 1 || f.p.b.f.Count(t, `SELECT count(*) FROM authorized_outbox WHERE operation='record-deployment'`) != 1 {
		t.Fatal("original atomic handoff unavailable")
	}
}
func TestCompletionWithdrawalDuringOutboxWait(t *testing.T) {
	f, _ := completedDeployment(t)
	ctx := context.Background()
	tx, e := f.p.b.f.Admin.Pool.BeginTx(ctx, nil)
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
	go func() { _, e := completionflow.DispatchDeployed(ctx, f.j.DB, completionLoader(f)); done <- e }()
	blocked := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e = f.p.b.f.Admin.Pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE usename='aisdlc_deployer' AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked); e != nil {
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
	if _, e = f.p.b.f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e == nil {
		t.Fatal("withdrawn completion dispatched after wait")
	}
	if f.p.b.f.Count(t, `SELECT count(*) FROM completion_handoffs`) != 0 || f.p.b.f.Count(t, `SELECT count(*) FROM authorized_outbox WHERE operation='record-deployment'`) != 0 {
		t.Fatal("partial withdrawn handoff survived")
	}
}
