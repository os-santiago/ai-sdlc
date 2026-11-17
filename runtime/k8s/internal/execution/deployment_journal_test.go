package execution

import (
	"context"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/deployment"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type deploymentFixture struct {
	p                promotionFixture
	j                *store.DeploymentJournal
	command          deployment.Command
	payload, receipt []byte
}

func newDeploymentFixture(t *testing.T) deploymentFixture {
	t.Helper()
	p := newPromotionFixture(t)
	ctx := context.Background()
	claim := p.claim(t)
	if e := p.j.FinishPromotion(ctx, claim, p.plan); e != nil {
		t.Fatal(e)
	}
	record, e := p.j.ReadPromotionRecord(ctx, claim.ID)
	if e != nil {
		t.Fatal(e)
	}
	command, e := deployment.NewCommand(*p.cfg, record, p.record, p.built, time.Now())
	if e != nil {
		t.Fatal("new deployment", e)
	}
	raw, _ := json.Marshal(command)
	signed, e := p.b.f.Material.Identities["release-manager"].Seal(p.cfg.Provenance.Build.Outer, command.ID(), "deploy-release", raw)
	if e != nil {
		t.Fatal(e)
	}
	receipt, _ := json.Marshal(signed)
	if e = p.b.f.Admin.ProvisionRolesForDeployment(ctx, p.b.passwords, "verification", true, true, true, true, true, true, true); e != nil {
		t.Fatal(e)
	}
	u, _ := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	u.User = url.UserPassword("aisdlc_deployer", p.b.passwords["deployer"])
	db, e := store.Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = db.Close() })
	return deploymentFixture{p, store.NewDeploymentJournal(db, func() (releasing.Configuration, error) { return *p.cfg, nil }), command, raw, receipt}
}
func (f deploymentFixture) claim(t *testing.T) store.DeploymentClaim {
	t.Helper()
	ctx := context.Background()
	if e := f.j.QueueDeployment(ctx, f.payload, f.receipt); e != nil {
		t.Fatal("queue", e)
	}
	c, e := f.j.ClaimDeployment(ctx, f.command.ID())
	if e != nil {
		t.Fatal("claim", e)
	}
	return c
}
func (f deploymentFixture) before() releasing.Snapshot {
	return releasing.Snapshot{DeploymentUID: f.p.cfg.Target.DeploymentUID, ResourceVersion: "100", Generation: 1, Image: f.p.cfg.Target.BaselineImage, TargetSHA256: f.p.cfg.Target.Digest(), ObservedAt: time.Now().UnixMilli()}
}
func (f deploymentFixture) effect(t *testing.T, c store.DeploymentClaim, before releasing.Snapshot, rollback bool) releasing.Receipt {
	t.Helper()
	ctx := context.Background()
	if e := f.j.BeginDeploymentEffect(ctx, c, before, rollback); e != nil {
		t.Fatal("intent", e)
	}
	image := f.p.plan.Image
	if rollback {
		image = f.p.cfg.Target.BaselineImage
	}
	raw, e := deployment.CompilePatch(f.p.cfg.Target, before, image, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	hash := authority.Digest(raw)
	gate := store.DeploymentGate{Journal: f.j, Claim: c}
	if e = gate.CheckConsoleEffect(ctx, before, hash, rollback); e != nil {
		t.Fatal("send gate", e)
	}
	if gate.CheckConsoleEffect(ctx, before, hash, rollback) == nil {
		t.Fatal("send replay")
	}
	after := before
	after.Generation++
	after.ResourceVersion = "200"
	if rollback {
		after.ResourceVersion = "300"
	}
	after.Image = image
	after.ObservedAt = time.Now().UnixMilli()
	return releasing.Receipt{Before: before, After: after, RequestSHA256: hash, Rollback: rollback}
}
func (f deploymentFixture) health(r releasing.Receipt) releasing.Health {
	image := strings.Split(r.After.Image, "@")[1]
	if r.Rollback {
		image = f.p.cfg.Target.BaselineRuntimeDigest
	}
	now := time.Now().UnixMilli()
	return releasing.Health{Version: "ai-sdlc.saferus-health/v1", Rollout: releasing.Rollout{DeploymentUID: r.After.DeploymentUID, Generation: r.After.Generation, ReplicaSetUID: store.NewID(), PodUID: store.NewID(), PodIP: "10.244.0.2", ImageID: "registry-fixture.invalid/image@" + image, Node: "fixture-node", ServiceUID: f.p.cfg.Target.ServiceUID, ServiceIP: "10.96.0.2", EndpointSliceUID: store.NewID(), TargetSHA256: r.After.TargetSHA256, Image: r.After.Image, ObservedAt: now}, ResponseSHA256: []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}, ObservedAt: now}
}

// Synthetic observations exercise real PostgreSQL authority/atomicity, never
// Kubernetes deployment evidence. The adapter's kind fixture covers actual API.
func TestDeploymentJournalExclusiveIntentSendAndCompletion(t *testing.T) {
	f := newDeploymentFixture(t)
	ctx := context.Background()
	if e := f.j.QueueDeployment(ctx, f.payload, f.receipt); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	claims := make(chan store.DeploymentClaim, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, e := f.j.ClaimDeployment(ctx, f.command.ID()); e == nil {
				claims <- c
			}
		}()
	}
	wg.Wait()
	close(claims)
	var c store.DeploymentClaim
	n := 0
	for c = range claims {
		n++
	}
	if n != 1 {
		t.Fatal("nonexclusive claim", n)
	}
	r := f.effect(t, c, f.before(), false)
	if e := f.j.RecordDeploymentReceipt(ctx, c, r); e != nil {
		t.Fatal("ack", e)
	}
	if f.j.RecordDeploymentReceipt(ctx, c, r) == nil {
		t.Fatal("ack replay")
	}
	got, e := f.j.ReadDeploymentReceipt(ctx, c, false)
	if e != nil || got != r {
		t.Fatal("read ack", e)
	}
	if e = f.j.FinishDeployment(ctx, c, f.health(r), false); e != nil {
		t.Fatal("finish", e)
	}
	if f.p.b.f.Count(t, `SELECT count(*) FROM deployment_requests WHERE state='deployed'`) != 1 {
		t.Fatal("no deployed record")
	}
	if f.j.FinishDeployment(ctx, c, f.health(r), false) == nil || f.j.QueueDeployment(ctx, f.payload, f.receipt) == nil {
		t.Fatal("terminal replay")
	}
	for _, q := range []string{`UPDATE deployment_intents SET request_digest=request_digest`, `DELETE FROM deployment_sends`, `UPDATE deployment_receipts SET receipt=receipt`, `UPDATE deployment_health SET health=health`, `UPDATE deployment_requests SET payload=payload`, `UPDATE promotion_records SET plan=plan`, `UPDATE build_requests SET state='failed'`, `DELETE FROM provenance_records`} {
		if _, e = f.j.Pool.Exec(q); e == nil {
			t.Fatal("mutable authority/history", q)
		}
	}
	if e = f.p.b.f.Admin.ProvisionRolesForPromotion(ctx, f.p.b.passwords, "verification", true, true, true, true, true, true); e != nil {
		t.Fatal(e)
	}
	if _, e = f.j.Pool.Query(`SELECT * FROM deployment_requests`); e == nil {
		t.Fatal("disabled deployment retains access")
	}
}
func TestDeploymentJournalRollbackRequiresOwnedAckAndSurvivesFreeze(t *testing.T) {
	f := newDeploymentFixture(t)
	ctx := context.Background()
	c := f.claim(t)
	before := f.before()
	r := f.effect(t, c, before, false)
	current := r.After
	current.ResourceVersion = "205"
	current.ObservedAt = time.Now().UnixMilli()
	if f.j.BeginDeploymentEffect(ctx, c, current, true) == nil {
		t.Fatal("rollback without durable ACK")
	}
	f.p.cfg.Provenance.Build.Inner.Freeze = true
	f.p.cfg.Provenance.Build.Outer.Freeze = true
	if e := f.j.RecordDeploymentReceipt(ctx, c, r); e != nil {
		t.Fatal("fact lost after freeze", e)
	}
	if _, _, e := f.j.ReadDeploymentInput(ctx, c); e == nil {
		t.Fatal("advance after freeze")
	}
	if _, e := f.p.b.f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
		t.Fatal(e)
	}
	wrong := current
	wrong.Generation++
	if f.j.BeginDeploymentEffect(ctx, c, wrong, true) == nil {
		t.Fatal("rollback over later generation")
	}
	rollback := f.effect(t, c, current, true)
	if e := f.j.RecordDeploymentReceipt(ctx, c, rollback); e != nil {
		t.Fatal(e)
	}
	if e := f.j.FinishDeployment(ctx, c, f.health(rollback), true); e != nil {
		t.Fatal("bounded containment", e)
	}
	if f.p.b.f.Count(t, `SELECT count(*) FROM deployment_requests WHERE state='rolled-back'`) != 1 {
		t.Fatal("no containment result")
	}
}
func TestDeploymentJournalUncertainSendCannotBeRepeatedOrAdopted(t *testing.T) {
	f := newDeploymentFixture(t)
	ctx := context.Background()
	c := f.claim(t)
	before := f.before()
	if e := f.j.BeginDeploymentEffect(ctx, c, before, false); e != nil {
		t.Fatal(e)
	}
	raw, _ := deployment.CompilePatch(f.p.cfg.Target, before, f.p.plan.Image, time.Now())
	hash := authority.Digest(raw)
	db, dropped := executionCommitFault(t, 1, "deployer")
	fault := store.NewDeploymentJournal(db, func() (releasing.Configuration, error) { return *f.p.cfg, nil })
	e := (store.DeploymentGate{Journal: fault, Claim: c}).CheckConsoleEffect(ctx, before, hash, false)
	select {
	case <-dropped:
	default:
		t.Fatal("COMMIT response not dropped")
	}
	if e == nil {
		t.Fatal("lost commit accepted")
	}
	restarted := store.NewDeploymentJournal(f.j.DB, func() (releasing.Configuration, error) { return *f.p.cfg, nil })
	if (store.DeploymentGate{Journal: restarted, Claim: c}).CheckConsoleEffect(ctx, before, hash, false) == nil {
		t.Fatal("durable send replay")
	}
	if _, e = restarted.ClaimDeployment(ctx, c.ID); e == nil {
		t.Fatal("reclaim")
	}
	current := before
	current.Generation++
	current.Image = f.p.plan.Image
	current.ResourceVersion = "200"
	if restarted.BeginDeploymentEffect(ctx, c, current, true) == nil {
		t.Fatal("image equality adopted as ACK")
	}
	if e = restarted.QuarantineDeployment(ctx, c, "uncertain"); e != nil {
		t.Fatal(e)
	}
	if f.p.b.f.Count(t, `SELECT count(*) FROM deployment_sends`) != 1 || f.p.b.f.Count(t, `SELECT count(*) FROM deployment_receipts`) != 0 {
		t.Fatal("uncertainty altered facts")
	}
}
func TestDeploymentJournalRejectsForeignAuthorityAndHealth(t *testing.T) {
	for _, name := range []string{"freeze", "withdrawal", "hash", "ack", "health", "target"} {
		t.Run(name, func(t *testing.T) {
			f := newDeploymentFixture(t)
			ctx := context.Background()
			c := f.claim(t)
			before := f.before()
			switch name {
			case "freeze":
				f.p.cfg.Provenance.Build.Outer.Freeze = true
				if f.j.BeginDeploymentEffect(ctx, c, before, false) == nil {
					t.Fatal("freeze")
				}
			case "withdrawal":
				_, e := f.p.b.f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`)
				if e != nil {
					t.Fatal(e)
				}
				if f.j.BeginDeploymentEffect(ctx, c, before, false) == nil {
					t.Fatal("withdrawal")
				}
			case "hash":
				if e := f.j.BeginDeploymentEffect(ctx, c, before, false); e != nil {
					t.Fatal(e)
				}
				if (store.DeploymentGate{Journal: f.j, Claim: c}).CheckConsoleEffect(ctx, before, strings.Repeat("f", 64), false) == nil {
					t.Fatal("hash substitution")
				}
			case "ack":
				r := f.effect(t, c, before, false)
				r.After.Generation++
				if f.j.RecordDeploymentReceipt(ctx, c, r) == nil {
					t.Fatal("foreign ack")
				}
			case "health":
				r := f.effect(t, c, before, false)
				if e := f.j.RecordDeploymentReceipt(ctx, c, r); e != nil {
					t.Fatal(e)
				}
				h := f.health(r)
				h.Rollout.ServiceUID = store.NewID()
				if f.j.FinishDeployment(ctx, c, h, false) == nil {
					t.Fatal("foreign endpoint")
				}
			case "target":
				r := f.effect(t, c, before, false)
				if e := f.j.RecordDeploymentReceipt(ctx, c, r); e != nil {
					t.Fatal(e)
				}
				f.p.cfg.Target.ServiceUID = store.NewID()
				if f.j.BeginDeploymentEffect(ctx, c, r.After, true) == nil {
					t.Fatal("changed containment target")
				}
			}
			if f.p.b.f.Count(t, `SELECT count(*) FROM deployment_health`) != 0 {
				t.Fatal("invalid final evidence")
			}
		})
	}
}

func TestDeploymentCommandBindsHistoryAndCannotExtendPlan(t *testing.T) {
	f := newDeploymentFixture(t)
	ctx := context.Background()
	r, e := f.p.j.ReadPromotionRecord(ctx, f.p.command.ID())
	if e != nil {
		t.Fatal(e)
	}
	var original authority.Receipt
	if authority.DecodeStrict(f.receipt, &original) != nil {
		t.Fatal("receipt")
	}
	now := time.Now()
	if _, e = deployment.VerifyCommand(*f.p.cfg, original, f.payload, now); e != nil {
		t.Fatal(e)
	}
	if f.command.BindRecords(*f.p.cfg, r, f.p.record, f.p.built, now) != nil {
		t.Fatal("binding")
	}
	legacy := struct {
		Command       releasing.Command
		Configuration releasing.Configuration
		Plan          releasing.Plan
		RecordedAt    time.Time
	}{r.Command, r.Configuration, r.Plan, r.RecordedAt}
	raw, _ := json.Marshal(legacy)
	if authority.Digest(raw) != r.Digest() {
		t.Fatal("historical encoding changed")
	}
	for _, name := range []string{"history", "plan", "config", "expiry", "expired", "future", "alias", "missing", "duplicate", "unknown", "wrong-role", "source", "artifact"} {
		t.Run(name, func(t *testing.T) {
			c := f.command
			cfg := *f.p.cfg
			record := r
			at := now
			switch name {
			case "history":
				c.PromotionRecordSHA256 = strings.Repeat("f", 64)
			case "plan":
				c.PlanSHA256 = strings.Repeat("f", 64)
			case "config":
				cfg.Target.ServiceUID = store.NewID()
			case "expiry":
				c.ExpiresAt++
			case "expired":
				at = time.UnixMilli(c.ExpiresAt)
			case "future":
				c.IssuedAt = now.Add(time.Minute).UnixMilli()
			case "source":
				record.Plan.SourceSHA = strings.Repeat("e", 40)
			case "artifact":
				record.Plan.Image = "untrusted.invalid/x:latest"
			}
			raw, _ := json.Marshal(c)
			switch name {
			case "alias":
				raw = []byte(strings.Replace(string(raw), `"planSHA256"`, `"PlanSHA256"`, 1))
			case "missing":
				var obj map[string]any
				_ = json.Unmarshal(raw, &obj)
				delete(obj, "issuedAt")
				raw, _ = json.Marshal(obj)
			case "duplicate":
				raw = append(raw[:len(raw)-1], []byte(`,"version":"duplicate"}`)...)
			case "unknown":
				raw = append(raw[:len(raw)-1], []byte(`,"url":"https://untrusted.invalid"}`)...)
			}
			key := f.p.b.f.Material.Identities["release-manager"]
			op := "deploy-release"
			if name == "wrong-role" {
				key = f.p.b.f.Material.Identities["verifier"]
				op = "promote-artifact"
			}
			signed, e := key.Seal(f.p.cfg.Provenance.Build.Outer, c.ID(), op, raw)
			if e != nil {
				t.Fatal(e)
			}
			_, verify := deployment.VerifyCommand(cfg, signed, raw, at)
			bind := c.BindRecords(cfg, record, f.p.record, f.p.built, at)
			if verify == nil && bind == nil {
				t.Fatal("substitution accepted")
			}
		})
	}
	if _, e = deployment.NewCommand(*f.p.cfg, r, f.p.record, f.p.built, time.UnixMilli(r.Plan.ExpiresAt).Add(-releasing.MinimumEffectBudget+time.Millisecond)); e == nil {
		t.Fatal("insufficient effect budget")
	}
}

func TestDeploymentJournalRevocationDuringBlockedHistory(t *testing.T) {
	for _, phase := range []string{"queue", "claim", "intent", "send", "finish", "rollback"} {
		t.Run(phase, func(t *testing.T) {
			f := newDeploymentFixture(t)
			ctx := context.Background()
			var c store.DeploymentClaim
			before := f.before()
			var health releasing.Health
			var hash string
			if phase == "claim" {
				if e := f.j.QueueDeployment(ctx, f.payload, f.receipt); e != nil {
					t.Fatal(e)
				}
			} else if phase != "queue" {
				c = f.claim(t)
			}
			if phase == "send" {
				if e := f.j.BeginDeploymentEffect(ctx, c, before, false); e != nil {
					t.Fatal(e)
				}
				raw, _ := deployment.CompilePatch(f.p.cfg.Target, before, f.p.plan.Image, time.Now())
				hash = authority.Digest(raw)
			}
			if phase == "finish" || phase == "rollback" {
				r := f.effect(t, c, before, false)
				if e := f.j.RecordDeploymentReceipt(ctx, c, r); e != nil {
					t.Fatal(e)
				}
				health = f.health(r)
				if phase == "rollback" {
					before = r.After
					before.ObservedAt = time.Now().UnixMilli()
				}
			}
			var mu sync.Mutex
			j := store.NewDeploymentJournal(f.j.DB, func() (releasing.Configuration, error) { mu.Lock(); defer mu.Unlock(); return *f.p.cfg, nil })
			tx, e := f.p.b.f.Admin.Pool.BeginTx(ctx, nil)
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
					e = j.QueueDeployment(ctx, f.payload, f.receipt)
				case "claim":
					_, e = j.ClaimDeployment(ctx, f.command.ID())
				case "intent":
					e = j.BeginDeploymentEffect(ctx, c, before, false)
				case "send":
					e = (store.DeploymentGate{Journal: j, Claim: c}).CheckConsoleEffect(ctx, before, hash, false)
				case "finish":
					e = j.FinishDeployment(ctx, c, health, false)
				case "rollback":
					e = j.BeginDeploymentEffect(ctx, c, before, true)
				}
				done <- e
			}()
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
				t.Fatal("deployer did not observably wait on historical record")
			}
			mu.Lock()
			if phase == "rollback" {
				f.p.cfg.Target.ServiceUID = store.NewID()
			} else {
				f.p.cfg.Provenance.Build.Outer.Freeze = true
			}
			mu.Unlock()
			if e = tx.Commit(); e != nil {
				t.Fatal(e)
			}
			if e = <-done; e == nil {
				t.Fatal("authority changed during wait")
			}
			if f.p.b.f.Count(t, `SELECT count(*) FROM deployment_health`) != 0 {
				t.Fatal("revoked health completion")
			}
			if phase == "send" && f.p.b.f.Count(t, `SELECT count(*) FROM deployment_sends`) != 0 {
				t.Fatal("revoked send committed")
			}
			if phase == "intent" && f.p.b.f.Count(t, `SELECT count(*) FROM deployment_intents`) != 0 {
				t.Fatal("revoked intent committed")
			}
		})
	}
}

func TestDeploymentJournalLostFactAndCompletionAcknowledgements(t *testing.T) {
	for _, phase := range []string{"ack", "finish"} {
		t.Run(phase, func(t *testing.T) {
			f := newDeploymentFixture(t)
			ctx := context.Background()
			c := f.claim(t)
			r := f.effect(t, c, f.before(), false)
			if phase == "finish" {
				if e := f.j.RecordDeploymentReceipt(ctx, c, r); e != nil {
					t.Fatal(e)
				}
			}
			db, dropped := executionCommitFault(t, 1, "deployer")
			fault := store.NewDeploymentJournal(db, func() (releasing.Configuration, error) { return *f.p.cfg, nil })
			var e error
			if phase == "ack" {
				e = fault.RecordDeploymentReceipt(ctx, c, r)
			} else {
				e = fault.FinishDeployment(ctx, c, f.health(r), false)
			}
			if e == nil {
				t.Fatal("COMMIT response loss accepted")
			}
			select {
			case <-dropped:
			default:
				t.Fatal("actual COMMIT response not dropped")
			}
			restarted := store.NewDeploymentJournal(f.j.DB, func() (releasing.Configuration, error) { return *f.p.cfg, nil })
			got, e := restarted.ReadDeploymentReceipt(ctx, c, false)
			if e != nil || got != r {
				t.Fatal("original durable acknowledgement unavailable", e)
			}
			if phase == "finish" {
				outcome, e := restarted.ReadDeploymentRecord(ctx, c)
				if e != nil || outcome.Outcome != "deployed" || outcome.Receipt != r {
					t.Fatal("terminal proof unavailable", e)
				}
				f.p.cfg.Provenance.Build.Outer.Freeze = true
				again, e := restarted.ReadDeploymentRecord(ctx, c)
				if e != nil || again.Digest() != outcome.Digest() {
					t.Fatal("history changed with current authority", e)
				}
			}
			if _, e = restarted.ClaimDeployment(ctx, c.ID); e == nil {
				t.Fatal("reclaim after durable facts")
			}
		})
	}
}

func TestDeploymentCommandPreservesSubmillisecondHistoryOrder(t *testing.T) {
	f := newDeploymentFixture(t)
	record, e := f.p.j.ReadPromotionRecord(context.Background(), f.command.PromotionID)
	if e != nil {
		t.Fatal(e)
	}
	base := time.Now().Add(time.Millisecond).Truncate(time.Millisecond)
	record.RecordedAt = base.Add(250 * time.Microsecond)
	now := base.Add(750 * time.Microsecond)
	command, e := deployment.NewCommand(*f.p.cfg, record, f.p.record, f.p.built, now)
	if e != nil {
		t.Fatal("earlier promotion in same millisecond rejected", e)
	}
	if command.IssuedAt != base.Add(time.Millisecond).UnixMilli() {
		t.Fatal("issuance did not retain ordering at wire precision")
	}
	if command.ExpiresAt != record.Plan.ExpiresAt {
		t.Fatal("wire precision extended original expiry")
	}
	if e = command.BindRecords(*f.p.cfg, record, f.p.record, f.p.built, time.UnixMilli(command.IssuedAt)); e != nil {
		t.Fatal("historical issuance boundary rejected", e)
	}
	record.RecordedAt = now
	if _, e = deployment.NewCommand(*f.p.cfg, record, f.p.record, f.p.built, now); e != nil {
		t.Fatal("same-instant submillisecond promotion rejected", e)
	}
	record.RecordedAt = now.Add(time.Microsecond)
	if _, e = deployment.NewCommand(*f.p.cfg, record, f.p.record, f.p.built, now); e == nil {
		t.Fatal("future promotion accepted within same millisecond")
	}
}

// This waits for the actual 45-second freshness window; no fixture clock or
// persisted timestamp is rewritten. An observed PostgreSQL lock delays INSERT.
func TestDeploymentHealthExpiryWhileInsertWaits(t *testing.T) {
	f := newDeploymentFixture(t)
	ctx := context.Background()
	claim := f.claim(t)
	receipt := f.effect(t, claim, f.before(), false)
	if e := f.j.RecordDeploymentReceipt(ctx, claim, receipt); e != nil {
		t.Fatal(e)
	}
	health := f.health(receipt)
	time.Sleep(time.Until(time.UnixMilli(health.ObservedAt).Add(43500 * time.Millisecond)))
	tx, e := f.p.b.f.Admin.Pool.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`LOCK TABLE deployment_health IN ACCESS EXCLUSIVE MODE`); e != nil {
		t.Fatal(e)
	}
	var blocker int
	if e = tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&blocker); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- f.j.FinishDeployment(ctx, claim, health, false) }()
	blocked := false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if e = f.p.b.f.Admin.Pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE usename='aisdlc_deployer' AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)) AND query LIKE 'INSERT INTO deployment_health%')`, blocker).Scan(&blocked); e != nil {
			t.Fatal(e)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("health INSERT did not observably wait")
	}
	time.Sleep(time.Until(time.UnixMilli(health.ObservedAt).Add(46 * time.Second)))
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e == nil {
		t.Fatal("terminal state accepted health expired at persisted timestamp")
	}
	var count int
	if e = f.p.b.f.Admin.Pool.QueryRow(`SELECT count(*) FROM deployment_health WHERE request_id=$1`, claim.ID).Scan(&count); e != nil || count != 0 {
		t.Fatal("stale health survived transaction rollback", count, e)
	}
	if e = f.j.FinishDeployment(ctx, claim, f.health(receipt), false); e != nil {
		t.Fatal("fresh health failed after original INSERT rolled back", e)
	}
	if _, e = f.j.ReadDeploymentRecord(ctx, claim); e != nil {
		t.Fatal("completed record cannot be authenticated", e)
	}
}
