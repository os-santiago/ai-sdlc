package execution

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func browserJournalFixture(t *testing.T, duration time.Duration) (deploymentFixture, store.BrowserGate, browsercheck.Evidence) {
	t.Helper()
	f := newDeploymentFixture(t)
	claim := f.rawClaim(t)
	snapshot := f.before()
	route := f.health(releasing.Receipt{After: snapshot, Rollback: true}).Rollout
	request, e := store.BrowserRequest(claim, f.p.cfg.Browser, "baseline", snapshot, route, time.Now().Add(duration))
	if e != nil {
		t.Fatal(e)
	}
	gate := store.BrowserGate{Journal: f.j, Claim: claim, Phase: "baseline", Snapshot: snapshot, Rollout: route, Request: request}
	ctx := context.Background()
	if e = gate.BeforeCreate(ctx, strings.Repeat("a", 64)); e != nil {
		t.Fatal("browser intent/send", e)
	}
	pod := store.NewID()
	if e = gate.AcknowledgePod(ctx, request.RequestID, pod); e != nil {
		t.Fatal("browser Pod ACK", e)
	}
	now := time.Now().UnixMilli()
	result := browsercheck.Result{Version: browsercheck.ResultVersion, Status: "passed", RequestID: request.RequestID, RequestSHA256: request.Digest(), TargetSHA256: request.TargetSHA256, SnapshotSHA256: request.SnapshotSHA256, ImageDigest: request.ImageDigest, Expectation: request.Expectation, StartedAt: now, FinishedAt: now, Observations: []browsercheck.Observation{{Viewport: "desktop", ResponseSHA256: strings.Repeat("b", 64)}, {Viewport: "mobile", ResponseSHA256: strings.Repeat("c", 64)}}, NormalTLS: true, NetworkScopePreserved: true}
	evidence := browsercheck.Evidence{ProfileSHA256: f.p.cfg.Browser.Digest(), Request: request, Result: result, PodUID: pod, ContainerID: "containerd://" + strings.Repeat("d", 64), RuntimeDigest: f.p.cfg.Browser.RuntimeDigest, DeletedAt: now}
	return f, gate, evidence
}
func TestBrowserJournalOneUseClaimAndOriginalPod(t *testing.T) {
	f, g, evidence := browserJournalFixture(t, 25*time.Second)
	ctx := context.Background()
	if g.BeforeCreate(ctx, strings.Repeat("a", 64)) == nil {
		t.Fatal("replayed browser creation")
	}
	if g.AcknowledgePod(ctx, g.Request.RequestID, store.NewID()) == nil {
		t.Fatal("adopted replacement Pod")
	}
	foreign := evidence
	foreign.PodUID = store.NewID()
	if g.RecordEvidence(ctx, foreign) == nil {
		t.Fatal("accepted foreign Pod evidence")
	}
	foreign = evidence
	foreign.Request.Expectation = "readable-duration"
	if g.RecordEvidence(ctx, foreign) == nil {
		t.Fatal("accepted foreign expectation")
	}
	if e := g.RecordEvidence(ctx, evidence); e != nil {
		t.Fatal("record observed evidence", e)
	}
	if g.RecordEvidence(ctx, evidence) == nil {
		t.Fatal("replayed browser evidence")
	}
	if f.p.b.f.Count(t, `SELECT count(*) FROM deployment_browser_evidence`) != 1 {
		t.Fatal("evidence cardinality")
	}
	for _, query := range []string{`UPDATE deployment_browser_intents SET request=request`, `DELETE FROM deployment_browser_evidence`, `UPDATE deployment_browser_pods SET pod_uid=pod_uid`} {
		if _, e := f.j.Pool.Exec(query); e == nil {
			t.Fatal("mutable browser history", query)
		}
	}
}
func TestBrowserJournalCannotCommitEvidenceAfterOriginalDeadline(t *testing.T) {
	f, g, evidence := browserJournalFixture(t, 11*time.Second)
	ctx := context.Background()
	// This trigger exists only in the disposable PostgreSQL fixture. It blocks
	// after RLS/default timestamps evaluated, so the late-commit guard must run.
	_, e := f.p.b.f.Admin.Pool.Exec(`CREATE FUNCTION public.browser_record_pause() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(81927831); RETURN NEW; END $$; CREATE TRIGGER browser_record_pause AFTER INSERT ON deployment_browser_evidence FOR EACH ROW EXECUTE FUNCTION public.browser_record_pause()`)
	if e != nil {
		t.Fatal(e)
	}
	lock, e := f.p.b.f.Admin.Pool.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Rollback()
	if _, e = lock.ExecContext(ctx, `SELECT pg_advisory_xact_lock(81927831)`); e != nil {
		t.Fatal(e)
	}
	wait := time.Until(time.UnixMilli(g.Request.ExpiresAt).Add(-700 * time.Millisecond))
	if wait > 0 {
		time.Sleep(wait)
	}
	finished := make(chan error, 1)
	go func() { finished <- g.RecordEvidence(ctx, evidence) }()
	observed := false
	end := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(end) {
		var count int
		if f.p.b.f.Admin.Pool.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objid=81927831 AND NOT granted`).Scan(&count) == nil && count == 1 {
			observed = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !observed {
		t.Fatal("did not observe the original INSERT waiting after its row was accepted")
	}
	time.Sleep(time.Until(time.UnixMilli(g.Request.ExpiresAt).Add(30 * time.Millisecond)))
	if e = lock.Rollback(); e != nil {
		t.Fatal(e)
	}
	select {
	case e = <-finished:
		if e == nil {
			t.Fatal("committed expired browser evidence")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bounded journal call did not finish")
	}
	if f.p.b.f.Count(t, `SELECT count(*) FROM deployment_browser_evidence`) != 0 {
		t.Fatal("expired evidence row survived")
	}
}

func TestBrowserJournalRequiredBeforeDeploymentAndTerminalHealth(t *testing.T) {
	f := newDeploymentFixture(t)
	c := f.rawClaim(t)
	ctx := context.Background()
	before := f.before()
	if f.j.BeginDeploymentEffect(ctx, c, before, false) == nil {
		t.Fatal("deployment began without baseline browser evidence")
	}
	f.ensureBaseline(t, c, before)
	receipt := f.effect(t, c, f.before(), false)
	if e := f.j.RecordDeploymentReceipt(ctx, c, receipt); e != nil {
		t.Fatal(e)
	}
	if f.j.FinishDeployment(ctx, c, f.health(receipt), false) == nil {
		t.Fatal("HTTP-only health completed deployment")
	}
	health := f.attestedHealth(t, c, receipt)
	forged := health
	forged.Browser.PodUID = store.NewID()
	if f.j.FinishDeployment(ctx, c, forged, false) == nil {
		t.Fatal("foreign browser Pod completed deployment")
	}
	forged = health
	forged.StartedAt -= 30000
	if f.j.FinishDeployment(ctx, c, forged, false) == nil {
		t.Fatal("extended combined smoke budget")
	}
	if e := f.j.FinishDeployment(ctx, c, health, false); e != nil {
		t.Fatal("authenticated browser completion", e)
	}
	record, e := f.j.ReadDeploymentRecord(ctx, c)
	if e != nil || record.Health.Browser.Digest() != health.Browser.Digest() {
		t.Fatal("lost browser chain in historical reader", e)
	}
}

func TestBrowserCleanupUsesFreshContainmentWithoutRenewingWorkflow(t *testing.T) {
	f, g, _ := browserJournalFixture(t, 11*time.Second)
	ctx := context.Background()
	if pending, e := f.j.PendingBrowserCleanup(ctx); e != nil || !pending {
		t.Fatal("missing pending containment", e)
	}
	if selected, e := f.j.NextBrowserCleanup(ctx); e != nil || selected.Request.RequestID != "" {
		t.Fatal("early cleanup selection", e)
	}
	runtime, _ := deploymentRuntime(f, t, "success")
	if state, e := runtime.PollOnce(ctx); e != nil || state != "browser-cleanup-pending" {
		t.Fatal("new work bypassed original browser window", state, e)
	}
	if e := f.j.QuarantineDeployment(ctx, g.Claim, "unhealthy"); e != nil {
		t.Fatal(e)
	}
	f.p.cfg.Provenance.Build.Outer.Freeze = true
	// Real original browser expiry; no timestamps or history rows are rewritten.
	time.Sleep(time.Until(time.UnixMilli(g.Request.ExpiresAt).Add(20 * time.Millisecond)))
	original, e := f.j.NextBrowserCleanup(ctx)
	if e != nil || original.PodUID == "" {
		t.Fatal("known cleanup selection", e)
	}
	start := time.Now()
	until := start.Add(5 * time.Second)
	lease, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	cleanup := store.BrowserCleanupGate{Journal: f.j, Original: original, StartedAt: start, Until: until}
	if e = cleanup.CheckCleanup(lease); e != nil {
		t.Fatal("contained resource incorrectly requires advance policy", e)
	}
	wrong := cleanup
	wrong.Original.PodUID = store.NewID()
	if wrong.CheckCleanup(lease) == nil {
		t.Fatal("replacement Pod acquired cleanup authority")
	}
	cancelled, stop := context.WithCancel(lease)
	stop()
	if cleanup.CheckCleanup(cancelled) == nil {
		t.Fatal("lost controller session retained cleanup authority")
	}
	if state, e := runtime.PollOnce(ctx); e != nil || state != "browser-cleanup-recorded" {
		t.Fatal("independent cleanup", state, e)
	}
	if cleanup.RecordCleanup(lease) == nil {
		t.Fatal("replayed containment fact")
	}
	if pending, e := f.j.PendingBrowserCleanup(ctx); e != nil || pending {
		t.Fatal("contained Pod still blocks future work", e)
	}
	if f.p.b.f.Count(t, `SELECT count(*) FROM deployment_browser_cleanups`) != 1 || f.p.b.f.Count(t, `SELECT count(*) FROM deployment_browser_evidence`) != 0 || f.p.b.f.Count(t, `SELECT count(*) FROM deployment_health`) != 0 {
		t.Fatal("cleanup invented health or failed to retain absence")
	}
	var state string
	if f.j.Pool.QueryRow(`SELECT state FROM deployment_requests WHERE id=$1`, g.Claim.ID).Scan(&state) != nil || state != "quarantined" {
		t.Fatal("cleanup advanced original deployment")
	}
	for _, query := range []string{`UPDATE deployment_browser_cleanups SET pod_uid=pod_uid`, `DELETE FROM deployment_browser_cleanups`} {
		if _, e = f.j.Pool.Exec(query); e == nil {
			t.Fatal("mutable cleanup facts")
		}
	}
}

func TestBrowserCreateWithoutOriginalACKRemainsExplicitlyQuarantined(t *testing.T) {
	f := newDeploymentFixture(t)
	c := f.rawClaim(t)
	snapshot := f.before()
	route := f.health(releasing.Receipt{After: snapshot, Rollback: true}).Rollout
	request, e := store.BrowserRequest(c, f.p.cfg.Browser, "baseline", snapshot, route, time.Now().Add(25*time.Second))
	if e != nil {
		t.Fatal(e)
	}
	g := store.BrowserGate{Journal: f.j, Claim: c, Phase: "baseline", Snapshot: snapshot, Rollout: route, Request: request}
	if e = g.BeforeCreate(context.Background(), strings.Repeat("a", 64)); e != nil {
		t.Fatal(e)
	}
	runtime, transport := deploymentRuntime(f, t, "success")
	if state, e := runtime.PollOnce(context.Background()); e == nil || state != "browser-create-unacknowledged" {
		t.Fatal("unknown Pod was adopted or replayed", state, e)
	}
	if transport.applies != 0 || transport.rollbacks != 0 || f.p.b.f.Count(t, `SELECT count(*) FROM deployment_browser_cleanups`) != 0 {
		t.Fatal("unknown Pod acquired containment or application effects")
	}
}
