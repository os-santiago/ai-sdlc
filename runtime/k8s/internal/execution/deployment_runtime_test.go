package execution

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/deployment"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/deploymentflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releaseflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Synthetic API observations exercise real authenticated dispatch/inbox/journal.
// They are controller-contract tests, never Kubernetes or vault evidence.
type deploymentTransportFixture struct {
	f                  deploymentFixture
	t                  *testing.T
	mode               string
	current            releasing.Snapshot
	applies, rollbacks int
	last               releasing.Receipt
	cancel             context.CancelFunc
}

func (d *deploymentTransportFixture) deadline(ctx context.Context) {
	d.t.Helper()
	until, ok := ctx.Deadline()
	if !ok || until.After(time.UnixMilli(d.f.command.ExpiresAt)) {
		d.t.Fatal("network operation outlives original command")
	}
}
func (d *deploymentTransportFixture) Observe(ctx context.Context) (releasing.Snapshot, error) {
	d.deadline(ctx)
	d.current.ObservedAt = time.Now().UnixMilli()
	return d.current, nil
}
func (d *deploymentTransportFixture) Apply(ctx context.Context, p releasing.Plan, before releasing.Snapshot, g podtransport.ConsoleEffectGate) (releasing.Receipt, error) {
	d.deadline(ctx)
	raw, e := deployment.CompilePatch(d.f.p.cfg.Target, before, p.Image, time.Now())
	if e != nil {
		return releasing.Receipt{}, e
	}
	hash := authority.Digest(raw)
	if e = g.CheckConsoleEffect(ctx, before, hash, false); e != nil {
		return releasing.Receipt{}, e
	}
	d.applies++
	after := before
	after.Generation++
	after.Image = p.Image
	after.ResourceVersion = "200"
	after.ObservedAt = time.Now().UnixMilli()
	d.current = after
	if d.mode == "lost-api-ack" {
		return releasing.Receipt{}, podtransport.ErrConsoleUncertain
	}
	r := releasing.Receipt{Before: before, After: after, RequestSHA256: hash}
	d.last = r
	switch d.mode {
	case "freeze":
		d.f.p.cfg.Provenance.Build.Outer.Freeze = true
	case "withdrawn":
		if _, e = d.f.p.b.f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
			d.t.Fatal(e)
		}
	case "target":
		d.f.p.cfg.Target.ServiceUID = store.NewID()
	case "cancel":
		d.cancel()
	}
	return r, nil
}
func (d *deploymentTransportFixture) Rollback(ctx context.Context, original releasing.Receipt, current releasing.Snapshot, g podtransport.ConsoleEffectGate) (releasing.Receipt, error) {
	d.deadline(ctx)
	raw, e := deployment.CompilePatch(d.f.p.cfg.Target, current, d.f.p.cfg.Target.BaselineImage, time.Now())
	if e != nil {
		return releasing.Receipt{}, e
	}
	hash := authority.Digest(raw)
	if e = g.CheckConsoleEffect(ctx, current, hash, true); e != nil {
		return releasing.Receipt{}, e
	}
	d.rollbacks++
	after := current
	after.Generation++
	after.Image = d.f.p.cfg.Target.BaselineImage
	after.ResourceVersion = "300"
	after.ObservedAt = time.Now().UnixMilli()
	d.current = after
	if d.mode == "lost-rollback-ack" {
		return releasing.Receipt{}, podtransport.ErrConsoleUncertain
	}
	r := releasing.Receipt{Before: current, After: after, RequestSHA256: hash, Rollback: true}
	d.last = r
	return r, nil
}
func (d *deploymentTransportFixture) WaitReady(ctx context.Context, s releasing.Snapshot) (releasing.Rollout, error) {
	d.deadline(ctx)
	if s.Image == d.f.p.plan.Image && (d.mode == "readiness" || d.mode == "lost-rollback-ack") {
		return releasing.Rollout{}, errors.New("synthetic rollout failure")
	}
	return d.f.health(d.last).Rollout, nil
}
func (d *deploymentTransportFixture) VerifyHealth(ctx context.Context, s releasing.Snapshot) (releasing.Health, error) {
	d.deadline(ctx)
	if s.Image == d.f.p.plan.Image && d.mode == "health" {
		return releasing.Health{}, errors.New("synthetic health failure")
	}
	receipt := d.last
	if receipt.After.Image == "" {
		receipt = releasing.Receipt{After: s, Rollback: true}
	}
	return d.f.health(receipt), nil
}
func deploymentRuntime(f deploymentFixture, t *testing.T, mode string) (deploymentflow.Runtime, *deploymentTransportFixture) {
	transport := &deploymentTransportFixture{f: f, t: t, mode: mode, current: f.before()}
	r := deploymentflow.Runtime{DB: f.j.DB, Load: func() (releasing.Configuration, error) { return *f.p.cfg, nil }, Target: func() (releasing.Target, error) { return f.p.cfg.Target, nil }, Browser: func() (browsercheck.Profile, error) { return f.p.cfg.Browser, nil }, CleanupTarget: func() (releasing.Target, error) { return f.p.cfg.Target, nil }, Cleaner: func() (deploymentflow.BrowserCleaner, error) { return transport, nil }, Transport: func(releasing.Target) (deploymentflow.Transport, error) { return transport, nil }}
	return r, transport
}
func TestDeploymentSignedHandoffInboxAndRuntime(t *testing.T) {
	f := newDeploymentFixture(t)
	ctx := context.Background()
	load := func() (releasing.Configuration, authority.Identity, error) {
		return *f.p.cfg, f.p.b.f.Material.Identities["release-manager"], nil
	}
	if sent, e := deploymentflow.DispatchPlanned(ctx, f.p.j.DB, load); e != nil || !sent {
		t.Fatal("dispatch", e)
	}
	if sent, e := deploymentflow.DispatchPlanned(ctx, f.p.j.DB, load); e != nil || sent {
		t.Fatal("dispatch replay", e)
	}
	var raw, payload []byte
	if e := f.p.j.Pool.QueryRow(`SELECT payload,receipt FROM authorized_outbox WHERE operation='deploy-release'`).Scan(&payload, &raw); e != nil {
		t.Fatal(e)
	}
	var signed authority.Receipt
	if authority.DecodeStrict(raw, &signed) != nil {
		t.Fatal("signed outbox")
	}
	command, e := deployment.VerifyCommand(*f.p.cfg, signed, payload, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	inbox := delivery.Inbox{DB: f.j.DB, Target: "deployer"}
	envelope := delivery.Envelope{Receipt: signed, Payload: payload}
	effect := deploymentflow.InboxEffect(f.j)
	if _, _, e = inbox.Accept(ctx, f.p.cfg.Provenance.Build.Outer, envelope, "deploy-release", http.MethodPost, "/internal/deploy-release", time.Now(), effect); e == nil {
		t.Fatal("unauthenticated transport")
	}
	for n := 0; n < 2; n++ {
		envelope.Transport, e = f.p.b.f.Material.Identities["relay"].Carry(f.p.cfg.Provenance.Build.Outer, signed, http.MethodPost, "/internal/deploy-release", time.Now())
		if e != nil {
			t.Fatal(e)
		}
		_, dup, e := inbox.Accept(ctx, f.p.cfg.Provenance.Build.Outer, envelope, "deploy-release", http.MethodPost, "/internal/deploy-release", time.Now(), effect)
		if e != nil || dup != (n == 1) {
			t.Fatal("inbox", dup, e)
		}
	}
	r, transport := deploymentRuntime(f, t, "success")
	if state, e := r.PollOnce(ctx); e != nil || state != "deployment-recorded" {
		t.Fatal("controller", state, e)
	}
	if state, e := r.PollOnce(ctx); e != nil || state != "deployment-idle" {
		t.Fatal("controller replay", state, e)
	}
	if transport.applies != 1 || transport.rollbacks != 0 || f.p.b.f.Count(t, `SELECT count(*) FROM deployment_requests WHERE id='`+command.ID()+`' AND state='deployed'`) != 1 {
		t.Fatal("effect cardinality")
	}
}
func TestDeploymentRuntimeContainsFailuresAndQuarantinesAmbiguity(t *testing.T) {
	for _, mode := range []string{"success", "readiness", "health", "freeze", "withdrawn", "target", "lost-api-ack", "lost-rollback-ack", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newDeploymentFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if e := f.j.QueueDeployment(ctx, f.payload, f.receipt); e != nil {
				t.Fatal(e)
			}
			r, transport := deploymentRuntime(f, t, mode)
			transport.cancel = cancel
			state, e := r.PollOnce(ctx)
			expected := "rolled-back"
			rollback := 1
			switch mode {
			case "success":
				expected = "deployed"
				rollback = 0
			case "target", "lost-api-ack", "cancel":
				expected = "quarantined"
				rollback = 0
			case "lost-rollback-ack":
				expected = "quarantined"
			}
			if expected == "quarantined" && e == nil {
				t.Fatal("uncertainty accepted", state)
			}
			if expected != "quarantined" && e != nil {
				t.Fatal("controller", state, e)
			}
			if transport.applies != 1 || transport.rollbacks != rollback || f.p.b.f.Count(t, `SELECT count(*) FROM deployment_requests WHERE state='`+expected+`'`) != 1 {
				t.Fatal("unexpected effects or final state", state, transport.applies, transport.rollbacks)
			}
		})
	}
}
func TestDeploymentHandoffFailureIsAtomic(t *testing.T) {
	for _, mode := range []string{"outbox", "freeze", "target", "wrong-role", "lost-commit"} {
		t.Run(mode, func(t *testing.T) {
			f := newDeploymentFixture(t)
			ctx := context.Background()
			db := f.p.j.DB
			n := 0
			load := func() (releasing.Configuration, authority.Identity, error) {
				n++
				if n == 2 {
					if mode == "freeze" {
						f.p.cfg.Provenance.Build.Outer.Freeze = true
					}
					if mode == "target" {
						f.p.cfg.Target.ServiceUID = store.NewID()
					}
				}
				return *f.p.cfg, f.p.b.f.Material.Identities["release-manager"], nil
			}
			if mode == "outbox" {
				if _, e := f.p.b.f.Admin.Pool.Exec(`ALTER TABLE authorized_outbox ADD CONSTRAINT fixture_deny_deployment CHECK(operation<>'deploy-release')`); e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() {
					_, _ = f.p.b.f.Admin.Pool.Exec(`ALTER TABLE authorized_outbox DROP CONSTRAINT fixture_deny_deployment`)
				})
			}
			if mode == "wrong-role" {
				db = f.j.DB
			}
			var dropped <-chan struct{}
			if mode == "lost-commit" {
				db, dropped = executionCommitFault(t, 1, "release-manager")
			}
			if sent, e := deploymentflow.DispatchPlanned(ctx, db, load); e == nil || sent {
				t.Fatal("invalid dispatch")
			}
			expected := 0
			if mode == "lost-commit" {
				expected = 1
				select {
				case <-dropped:
				default:
					t.Fatal("COMMIT response not dropped")
				}
				if sent, e := deploymentflow.DispatchPlanned(ctx, f.p.j.DB, load); e != nil || sent {
					t.Fatal("restart replay", e)
				}
			}
			if f.p.b.f.Count(t, `SELECT count(*) FROM deployment_handoffs`) != expected || f.p.b.f.Count(t, `SELECT count(*) FROM authorized_outbox WHERE operation='deploy-release'`) != expected {
				t.Fatal("partial handoff")
			}
		})
	}
}
func TestDeploymentSelectorsExcludeWithdrawnIssues(t *testing.T) {
	f := newDeploymentFixture(t)
	ctx := context.Background()
	if id, e := f.p.j.NextPlannedForDeployment(ctx, *f.p.cfg); e != nil || id != f.p.command.ID() {
		t.Fatal("planned positive", e)
	}
	if e := f.j.QueueDeployment(ctx, f.payload, f.receipt); e != nil {
		t.Fatal(e)
	}
	if id, e := f.j.NextDeployment(ctx); e != nil || id != f.command.ID() {
		t.Fatal("queued positive", e)
	}
	if _, e := f.p.b.f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
		t.Fatal(e)
	}
	if id, e := f.p.j.NextPlannedForDeployment(ctx, *f.p.cfg); e != nil || id != "" {
		t.Fatal("withdrawn planned", e)
	}
	if id, e := f.j.NextDeployment(ctx); e != nil || id != "" {
		t.Fatal("withdrawn queued", e)
	}
}

func TestDeploymentRuntimeIndependentTargetLoaderForContainment(t *testing.T) {
	for _, mode := range []string{"valid-target", "missing-target", "changed-target", "expired-target"} {
		t.Run(mode, func(t *testing.T) {
			f := newDeploymentFixture(t)
			ctx := context.Background()
			if e := f.j.QueueDeployment(ctx, f.payload, f.receipt); e != nil {
				t.Fatal(e)
			}
			r, transport := deploymentRuntime(f, t, "success")
			r.Load = func() (releasing.Configuration, error) {
				if transport.applies > 0 {
					return releasing.Configuration{}, errors.New("advance configuration withdrawn")
				}
				return *f.p.cfg, nil
			}
			r.Target = func() (releasing.Target, error) {
				target := f.p.cfg.Target
				if transport.applies > 0 {
					switch mode {
					case "missing-target":
						return releasing.Target{}, errors.New("target unavailable")
					case "changed-target":
						target.ServiceUID = store.NewID()
					case "expired-target":
						target.ExpiresAt = time.Now().UnixMilli()
					}
				}
				return target, nil
			}
			state, e := r.PollOnce(ctx)
			if mode == "valid-target" {
				if e != nil || state != "deployment-rolled-back" || transport.rollbacks != 1 {
					t.Fatal("independent containment", state, e)
				}
			} else {
				if e == nil || transport.rollbacks != 0 || f.p.b.f.Count(t, `SELECT count(*) FROM deployment_requests WHERE state='quarantined'`) != 1 {
					t.Fatal("unavailable target accepted", state, e)
				}
			}
		})
	}
}

func TestDeploymentHandoffWithdrawalDuringOutboxWait(t *testing.T) {
	f := newDeploymentFixture(t)
	p := f.p
	ctx := context.Background()
	db := p.j.DB
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
		_, e := deploymentflow.DispatchPlanned(ctx, db, func() (releasing.Configuration, authority.Identity, error) {
			return *p.cfg, p.b.f.Material.Identities["release-manager"], nil
		})
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
	if p.b.f.Count(t, `SELECT count(*) FROM deployment_handoffs`) != 0 || p.b.f.Count(t, `SELECT count(*) FROM authorized_outbox WHERE operation='deploy-release'`) != 0 {
		t.Fatal("stale partial handoff")
	}
}

func TestDeploymentMountedTargetSurvivesOnlyAdvancePolicyWithdrawal(t *testing.T) {
	f := newDeploymentFixture(t)
	root := t.TempDir()
	cfg := *f.p.cfg
	write := func(name string, value any) string {
		t.Helper()
		path := filepath.Join(root, name)
		raw, e := json.Marshal(value)
		if e != nil || os.WriteFile(path, raw, 0600) != nil {
			t.Fatal("fixture file")
		}
		return path
	}
	build := buildflow.Installed{Version: "ai-sdlc.build-installation/v1", Inner: cfg.Provenance.Build.Inner, Outer: cfg.Provenance.Build.Outer, Profile: f.p.built.Context.Profile, Slots: cfg.Provenance.Build.Slots, IntakeConfigSHA256: cfg.Provenance.Build.IntakeConfigSHA256, IssueMaxAgeSeconds: cfg.Provenance.Build.IssueMaxAgeSeconds}
	mounts := releaseflow.Mounts{Build: write("build.json", build), Inner: write("inner.json", build.Inner), Outer: write("outer.json", build.Outer), Trust: write("trust.json", cfg.Provenance.Trust), Target: write("target.json", cfg.Target), Browser: write("browser.json", cfg.Browser), Identity: write("identity.json", f.p.b.f.Material.Identities["deployer"]), VerifierImage: cfg.Provenance.Trust.VerifierImage}
	loaded, key, e := mounts.Load("deployer")
	if e != nil || key.Role != "deployer" || deployment.ConfigurationDigest(loaded) != deployment.ConfigurationDigest(cfg) {
		t.Fatal("installed positive", e)
	}
	frozen := build.Outer
	frozen.Freeze = true
	write("outer.json", frozen)
	if _, _, e = mounts.Load("deployer"); e == nil {
		t.Fatal("stale advance snapshot accepted")
	}
	identity, e := mounts.LoadTargetIdentity()
	if e != nil || identity.Digest() != cfg.Target.Digest() {
		t.Fatal("cleanup identity incorrectly requires advance policy", e)
	}
	target, e := mounts.LoadTarget()
	if e != nil || target.Digest() != cfg.Target.Digest() {
		t.Fatal("target incorrectly requires advance authority", e)
	}
	for _, name := range []string{"missing", "malformed", "expired"} {
		t.Run(name, func(t *testing.T) {
			write("target.json", cfg.Target)
			switch name {
			case "missing":
				if e := os.Remove(mounts.Target); e != nil {
					t.Fatal(e)
				}
			case "malformed":
				if e := os.WriteFile(mounts.Target, []byte("{}"), 0600); e != nil {
					t.Fatal(e)
				}
			case "expired":
				x := cfg.Target
				x.ExpiresAt = time.Now().UnixMilli()
				write("target.json", x)
			}
			if _, e := mounts.LoadTarget(); e == nil {
				t.Fatal("cached/invalid target accepted")
			}
		})
	}
}

func TestBrowserCleanupTargetIdentityDoesNotRenewExpiredAppApproval(t *testing.T) {
	f := newDeploymentFixture(t)
	target := f.p.cfg.Target
	target.ApprovedAt = time.Now().Add(-time.Hour).UnixMilli()
	target.ExpiresAt = time.Now().Add(-time.Minute).UnixMilli()
	raw, _ := json.Marshal(target)
	path := filepath.Join(t.TempDir(), "target.json")
	if os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("target fixture")
	}
	mounts := releaseflow.Mounts{Target: path}
	if _, e := mounts.LoadTarget(); e == nil {
		t.Fatal("expired target regained application authority")
	}
	identity, e := mounts.LoadTargetIdentity()
	if e != nil || identity.Digest() != target.Digest() {
		t.Fatal("original cleanup identity unavailable after approval expiry", e)
	}
}
