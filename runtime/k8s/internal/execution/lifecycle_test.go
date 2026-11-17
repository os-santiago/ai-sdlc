package execution

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

type lifecycleFixture struct {
	*preparationFixture
	prepared  Prepared
	placement Placement
}

func newLifecycleFixture(t *testing.T, configure ...func(*fixture)) *lifecycleFixture {
	t.Helper()
	f := newPreparationFixture(t, configure...)
	_, err := f.Admin.Pool.Exec(`GRANT SELECT,INSERT ON execution_lifecycles,execution_effects TO aisdlc_worker;
 GRANT UPDATE(phase,config_uid,pod_uid,updated_at) ON execution_lifecycles TO aisdlc_worker;
 GRANT UPDATE(result,completed_at) ON execution_effects TO aisdlc_worker`)
	if err != nil {
		t.Fatal(err)
	}
	p, err := prepare(context.Background(), f.db, f.load, f.claim, f.source)
	if err != nil {
		t.Fatal(err)
	}
	placement := Placement{"fixture", "example.invalid/worker@sha256:" + strings.Repeat("a", 64), "example.invalid/mailbox@sha256:" + strings.Repeat("b", 64)}
	if f.placementOverride != nil {
		placement = *f.placementOverride
	}
	return &lifecycleFixture{f, p, placement}
}
func (f *lifecycleFixture) loadLifecycle(ctx context.Context) (PreparationInputs, Placement, error) {
	in, err := f.load(ctx)
	return in, f.placement, err
}
func (f *lifecycleFixture) open(t *testing.T) *Lifecycle {
	t.Helper()
	l, err := OpenLifecycle(context.Background(), f.db, f.loadLifecycle, f.prepared)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func TestLifecycleTransfersClaimOnceAndRevokesOldOwner(t *testing.T) {
	f := newLifecycleFixture(t)
	l := f.open(t)
	if l.durable.Claim.Token == f.claim.Token || !l.durable.Claim.Until.After(f.claim.Until) || l.durable.Claim.Until.After(time.Now().Add(150*time.Second)) {
		t.Fatal("unbounded or unrotated claim transfer")
	}
	if _, err := f.db.ReadClaimedExecution(context.Background(), f.claim); err == nil {
		t.Fatal("old claim remained usable")
	}
	if err := f.db.StopExecutionClaim(context.Background(), f.claim, "aborted"); err == nil {
		t.Fatal("old claimant stopped new owner")
	}
	if _, err := OpenLifecycle(context.Background(), f.db, f.loadLifecycle, f.prepared); err == nil {
		t.Fatal("preparation adopted twice")
	}
	if _, err := f.db.ReadClaimedExecution(context.Background(), l.durable.Claim); err != nil {
		t.Fatal("loser stopped winner")
	}
	if _, err := f.db.ReserveLifecycle(context.Background(), l.durable.Claim, l.durable.Binding); err == nil {
		t.Fatal("lifetime renewed")
	}
	if _, err := f.db.ReadClaimedExecution(context.Background(), l.durable.Claim); err != nil {
		t.Fatal("failed renewal changed original owner")
	}
}
func TestLifecycleConcurrentReservationHasOneLiveOwner(t *testing.T) {
	f := newLifecycleFixture(t)
	var wg sync.WaitGroup
	owners := make(chan *Lifecycle, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := OpenLifecycle(context.Background(), f.db, f.loadLifecycle, f.prepared)
			if err == nil {
				owners <- l
			}
		}()
	}
	wg.Wait()
	close(owners)
	var winner *Lifecycle
	count := 0
	for l := range owners {
		count++
		winner = l
	}
	if count != 1 {
		t.Fatalf("owners=%d", count)
	}
	if _, err := f.db.ReadClaimedExecution(context.Background(), winner.durable.Claim); err != nil {
		t.Fatal("loser stopped owner")
	}
	if f.Count(t, `SELECT count(*) FROM execution_lifecycles`) != 1 {
		t.Fatal("duplicate lifecycle")
	}
}
func TestLifecycleOrderedEffectsBindUIDsAndUntrustedCandidate(t *testing.T) {
	f := newLifecycleFixture(t)
	l := f.open(t)
	configUID, podUID := store.NewID(), store.NewID()
	zero := int32(0)
	calls := 0
	for _, kind := range []string{"config", "pod", "source", "start", "export"} {
		err := l.RunEffect(context.Background(), kind, func(ctx context.Context, in EffectInput) (store.LifecycleResult, error) {
			calls++
			deadline, ok := ctx.Deadline()
			if !ok || deadline.After(l.durable.Claim.Until) {
				t.Fatal("effect deadline")
			}
			if f.Count(t, `SELECT count(*) FROM execution_effects WHERE result IS NULL`) != 1 {
				t.Fatal("effect began without committed intent")
			}
			if in.Kind != kind {
				t.Fatal("wrong effect")
			}
			result := store.LifecycleResult{UID: podUID}
			switch kind {
			case "config":
				result.UID = configUID
			case "pod":
				if in.ConfigUID != configUID {
					t.Fatal("config identity lost")
				}
			case "source":
				if in.PodUID != podUID || workspacebundle.Digest(in.Source) != l.durable.Binding.SourceDigest {
					t.Fatal("source binding")
				}
				result.Digest = workspacebundle.Digest(in.Source)
				result.Bytes = len(in.Source)
				in.Source[0] ^= 1
			case "start":
				if in.PodUID != podUID {
					t.Fatal("pod identity lost")
				}
			case "export":
				result.Digest = strings.Repeat("c", 64)
				result.Bytes = 123
				result.ExitCode = &zero
			}
			// Caller mutation cannot modify future compiler inputs.
			in.Assignment[0] ^= 1
			in.Profile[0] ^= 1
			in.ADev[0] ^= 1
			return result, nil
		})
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	if calls != 5 || f.Count(t, `SELECT count(*) FROM execution_effects WHERE result IS NOT NULL`) != 5 || f.Count(t, `SELECT count(*) FROM execution_lifecycles WHERE phase='export-ready'`) != 1 {
		t.Fatal("incomplete ordered evidence")
	}
	if f.Count(t, `SELECT count(*) FROM attempts`) != 0 || f.Count(t, `SELECT count(*) FROM release_gates`) != 0 {
		t.Fatal("collection implied acceptance")
	}
}
func TestLifecycleDeniesOutOfOrderAndForeignResult(t *testing.T) {
	for _, which := range []string{"order", "uid", "error", "panic", "revoked", "late", "replay"} {
		t.Run(which, func(t *testing.T) {
			f := newLifecycleFixture(t)
			l := f.open(t)
			kind := "config"
			calls := 0
			if which == "order" {
				kind = "start"
			}
			operation := func(ctx context.Context, in EffectInput) (store.LifecycleResult, error) {
				calls++
				switch which {
				case "uid":
					return store.LifecycleResult{UID: "forged"}, nil
				case "error":
					return store.LifecycleResult{}, errors.New("fixture")
				case "panic":
					panic("fixture panic")
				case "revoked":
					f.mu.Lock()
					f.inputs.Policy.Autonomy = "manual"
					f.mu.Unlock()
				case "late":
					if err := f.db.StopExecutionClaim(ctx, l.durable.Claim, "aborted"); err != nil {
						t.Fatal(err)
					}
				}
				return store.LifecycleResult{UID: store.NewID()}, nil
			}
			err := l.RunEffect(context.Background(), kind, operation)
			if which == "replay" {
				if err != nil {
					t.Fatal(err)
				}
				err = l.RunEffect(context.Background(), kind, operation)
			}
			if err == nil {
				t.Fatal("unsafe effect accepted")
			}
			if which == "order" && calls != 0 {
				t.Fatal("out of order callback")
			}
			if which != "order" && calls != 1 {
				t.Fatal("callback repeated")
			}
			if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='stopped'`) != 1 {
				t.Fatal("unsafe owner not stopped")
			}
			if err = l.RunEffect(context.Background(), "config", operation); err == nil {
				t.Fatal("stopped owner resumed")
			}
		})
	}
}
func TestLifecycleSeparateObjectsCannotRepeatCommittedIntent(t *testing.T) {
	f := newLifecycleFixture(t)
	one := f.open(t)
	two := &Lifecycle{db: one.db, load: one.load, prepared: one.prepared, placement: one.placement, durable: one.durable}
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- one.RunEffect(context.Background(), "config", func(context.Context, EffectInput) (store.LifecycleResult, error) {
			calls.Add(1)
			close(started)
			<-release
			return store.LifecycleResult{UID: store.NewID()}, nil
		})
	}()
	<-started
	if err := two.RunEffect(context.Background(), "config", func(context.Context, EffectInput) (store.LifecycleResult, error) {
		calls.Add(1)
		return store.LifecycleResult{UID: store.NewID()}, nil
	}); err == nil {
		t.Fatal("second owner adopted intent")
	}
	close(release)
	if <-done == nil {
		t.Fatal("late result after stop accepted")
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate callback")
	}
}

func TestLifecycleLostReservationCommitReturnsNoOwner(t *testing.T) {
	f := newLifecycleFixture(t)
	faulted, dropped := executionCommitFault(t, 1)
	l, err := OpenLifecycle(context.Background(), faulted, f.loadLifecycle, f.prepared)
	if err == nil || l != nil {
		t.Fatal("lost reservation ACK exposed owner")
	}
	select {
	case <-dropped:
	default:
		t.Fatal("actual COMMIT ACK not dropped")
	}
	if f.Count(t, `SELECT count(*) FROM execution_lifecycles`) != 1 || f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='stopped' AND stop_reason='uncertain'`) != 1 {
		t.Fatal("rotated uncertain owner not stopped after reconnect")
	}
	if _, err = OpenLifecycle(context.Background(), f.db, f.loadLifecycle, f.prepared); err == nil {
		t.Fatal("uncertain owner adopted")
	}
}
func TestLifecycleLostEffectCommitsNeverReplay(t *testing.T) {
	for _, nth := range []int32{1, 2} {
		t.Run(map[int32]string{1: "intent", 2: "result"}[nth], func(t *testing.T) {
			f := newLifecycleFixture(t)
			l := f.open(t)
			faulted, dropped := executionCommitFault(t, nth)
			l.db = faulted
			calls := 0
			operation := func(context.Context, EffectInput) (store.LifecycleResult, error) {
				calls++
				return store.LifecycleResult{UID: store.NewID()}, nil
			}
			if err := l.RunEffect(context.Background(), "config", operation); err == nil {
				t.Fatal("lost effect ACK reported success")
			}
			select {
			case <-dropped:
			default:
				t.Fatal("actual COMMIT ACK not dropped")
			}
			if calls != int(nth-1) {
				t.Fatalf("callbacks=%d", calls)
			}
			if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='stopped' AND stop_reason='uncertain'`) != 1 {
				t.Fatal("uncertain effect owner not stopped")
			}
			expected := 0
			if nth == 2 {
				expected = 1
			}
			if f.Count(t, `SELECT count(*) FROM execution_effects WHERE result IS NOT NULL`) != expected {
				t.Fatal("committed evidence lost")
			}
			l.db = f.db
			if err := l.RunEffect(context.Background(), "config", operation); err == nil || calls != int(nth-1) {
				t.Fatal("uncertain effect replayed")
			}
		})
	}
}
func TestLifecycleRevalidatesAfterIntentCommit(t *testing.T) {
	for _, which := range []string{"authority", "placement", "generation", "cancel"} {
		t.Run(which, func(t *testing.T) {
			f := newLifecycleFixture(t)
			l := f.open(t)
			base := l.load
			calls := 0
			mutated := false
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			l.load = func(ctx context.Context) (PreparationInputs, Placement, error) {
				in, placement, err := base(ctx)
				if !mutated && f.Count(t, `SELECT count(*) FROM execution_effects`) != 0 {
					mutated = true
					switch which {
					case "authority":
						in.Policy.Autonomy = "manual"
					case "placement":
						placement.Namespace = "changed"
					case "generation":
						if _, e := f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=2`); e != nil {
							t.Fatal(e)
						}
					case "cancel":
						cancel()
					}
				}
				return in, placement, err
			}
			if err := l.RunEffect(ctx, "config", func(context.Context, EffectInput) (store.LifecycleResult, error) {
				calls++
				return store.LifecycleResult{UID: store.NewID()}, nil
			}); err == nil || calls != 0 || !mutated {
				t.Fatal("changed proof reached callback")
			}
			if f.Count(t, `SELECT count(*) FROM execution_effects WHERE result IS NULL`) != 1 {
				t.Fatal("intent evidence lost")
			}
		})
	}
}
func TestLifecycleFixtureGrantsCannotRewriteImmutableIdentity(t *testing.T) {
	f := newLifecycleFixture(t)
	l := f.open(t)
	for _, statement := range []string{
		`UPDATE execution_lifecycles SET binding_digest=repeat('a',64)`,
		`UPDATE execution_lifecycles SET claim_until=clock_timestamp()+interval '1 hour'`,
		`UPDATE execution_lifecycles SET claim_token='00000000-0000-0000-0000-000000000000'`,
		`UPDATE execution_effects SET token='00000000-0000-0000-0000-000000000000'`,
		`DELETE FROM execution_lifecycles`,
	} {
		if _, err := f.db.Pool.Exec(statement); err == nil {
			t.Fatal("immutable lifecycle statement allowed")
		}
	}
	if _, err := f.db.ReadClaimedExecution(context.Background(), l.durable.Claim); err != nil {
		t.Fatal(err)
	}
}

func (f *lifecycleFixture) shortenLifecycle(t *testing.T, l *Lifecycle) {
	t.Helper()
	var until time.Time
	if err := f.Admin.Pool.QueryRow(`UPDATE execution_requests SET claim_until=clock_timestamp()+interval '700 milliseconds' WHERE id=$1 RETURNING claim_until`, l.durable.Claim.ID).Scan(&until); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Admin.Pool.Exec(`UPDATE execution_lifecycles SET claim_until=$2 WHERE request_id=$1`, l.durable.Claim.ID, until); err != nil {
		t.Fatal(err)
	}
	l.durable.Claim.Until = until
	l.prepared.claim.Until = until
}
func (f *lifecycleFixture) waitForWorkerLock(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := f.Admin.Pool.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE usename='aisdlc_worker' AND wait_event_type='Lock'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("actual worker lock wait not observed")
}
func TestLifecycleExpiryAfterEveryDurableLock(t *testing.T) {
	for _, which := range []string{"request", "lifecycle", "effect"} {
		t.Run(which, func(t *testing.T) {
			f := newLifecycleFixture(t)
			l := f.open(t)
			var effect store.LifecycleEffect
			var err error
			if which == "effect" {
				effect, err = f.db.BeginLifecycleEffect(context.Background(), l.durable, "config")
				if err != nil {
					t.Fatal(err)
				}
			}
			f.shortenLifecycle(t, l)
			effect.Lifecycle = l.durable
			lock, err := f.Admin.Pool.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback()
			query := map[string]string{"request": `SELECT id FROM execution_requests WHERE id=$1 FOR UPDATE`, "lifecycle": `SELECT request_id FROM execution_lifecycles WHERE request_id=$1 FOR UPDATE`, "effect": `SELECT request_id FROM execution_effects WHERE request_id=$1 FOR UPDATE`}[which]
			if _, err = lock.Exec(query, l.durable.Claim.ID); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				if which == "effect" {
					done <- f.db.CompleteLifecycleEffect(context.Background(), effect, store.LifecycleResult{UID: store.NewID()})
				} else {
					_, err := f.db.BeginLifecycleEffect(context.Background(), l.durable, "config")
					done <- err
				}
			}()
			f.waitForWorkerLock(t)
			time.Sleep(time.Until(l.durable.Claim.Until) + 30*time.Millisecond)
			if err = lock.Commit(); err != nil {
				t.Fatal(err)
			}
			if <-done == nil {
				t.Fatal("expired owner acknowledged after row lock")
			}
			if f.Count(t, `SELECT count(*) FROM execution_effects WHERE result IS NOT NULL`) != 0 {
				t.Fatal("expired result persisted")
			}
		})
	}
}

func TestLifecycleOperationalRolesHaveNoAccess(t *testing.T) {
	f := newPreparationFixture(t)
	for _, table := range []string{"execution_lifecycles", "execution_effects", "execution_models", "execution_candidates"} {
		var allowed bool
		if err := f.Admin.Pool.QueryRow(`SELECT has_table_privilege('aisdlc_worker',$1,'SELECT,INSERT,UPDATE,DELETE')`, table).Scan(&allowed); err != nil {
			t.Fatal(err)
		}
		if allowed {
			t.Fatal("operational lifecycle access granted")
		}
	}
}
func TestLifecycleRejectsForeignEffectTokensAndPodUIDs(t *testing.T) {
	f := newLifecycleFixture(t)
	l := f.open(t)
	effect, err := f.db.BeginLifecycleEffect(context.Background(), l.durable, "config")
	if err != nil {
		t.Fatal(err)
	}
	for _, which := range []string{"effect-token", "claim-token", "binding", "kind"} {
		wrong := effect
		switch which {
		case "effect-token":
			wrong.Token = store.NewID()
		case "claim-token":
			wrong.Lifecycle.Claim.Token = store.NewID()
		case "binding":
			wrong.Lifecycle.Binding.BindingDigest = strings.Repeat("d", 64)
		case "kind":
			wrong.Kind = "pod"
		}
		if err = f.db.CompleteLifecycleEffect(context.Background(), wrong, store.LifecycleResult{UID: store.NewID()}); err == nil {
			t.Fatal("foreign effect completed")
		}
	}
	if err = f.db.CompleteLifecycleEffect(context.Background(), effect, store.LifecycleResult{UID: store.NewID()}); err != nil {
		t.Fatal(err)
	}
	podUID := store.NewID()
	effect, err = f.db.BeginLifecycleEffect(context.Background(), l.durable, "pod")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.CompleteLifecycleEffect(context.Background(), effect, store.LifecycleResult{UID: podUID}); err != nil {
		t.Fatal(err)
	}
	effect, err = f.db.BeginLifecycleEffect(context.Background(), l.durable, "source")
	if err != nil {
		t.Fatal(err)
	}
	result := store.LifecycleResult{UID: store.NewID(), Digest: l.durable.Binding.SourceDigest, Bytes: len(l.prepared.source)}
	if err = f.db.CompleteLifecycleEffect(context.Background(), effect, result); err == nil {
		t.Fatal("replacement Pod result accepted")
	}
	result.UID = podUID
	result.Digest = strings.Repeat("a", 64)
	if err = f.db.CompleteLifecycleEffect(context.Background(), effect, result); err == nil {
		t.Fatal("wrong source accepted")
	}
	if f.Count(t, `SELECT count(*) FROM execution_effects WHERE kind='source' AND result IS NULL`) != 1 {
		t.Fatal("invalid source consumed intent")
	}
}
func TestLifecycleResultPointerCannotChangeDuringProof(t *testing.T) {
	f := newLifecycleFixture(t)
	l := f.open(t)
	podUID := store.NewID()
	for _, kind := range []string{"config", "pod", "source", "start"} {
		if err := l.RunEffect(context.Background(), kind, func(context.Context, EffectInput) (store.LifecycleResult, error) {
			r := store.LifecycleResult{UID: podUID}
			if kind == "source" {
				r.Digest = l.durable.Binding.SourceDigest
				r.Bytes = len(l.prepared.source)
			}
			return r, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	code := int32(9)
	returned := false
	base := l.load
	l.load = func(ctx context.Context) (PreparationInputs, Placement, error) {
		if returned {
			code = 0
		}
		return base(ctx)
	}
	if err := l.RunEffect(context.Background(), "export", func(context.Context, EffectInput) (store.LifecycleResult, error) {
		returned = true
		return store.LifecycleResult{UID: podUID, Digest: strings.Repeat("a", 64), Bytes: 12, ExitCode: &code}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := f.Admin.Pool.QueryRow(`SELECT result FROM execution_effects WHERE kind='export'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"exitCode":9`) {
		t.Fatal("callback-owned pointer changed recorded result")
	}
}

func TestLifecycleReservationCannotExceedCommandExpiry(t *testing.T) {
	f := newLifecycleFixture(t)
	var expires time.Time
	if err := f.Admin.Pool.QueryRow(`UPDATE execution_requests SET expires_at=clock_timestamp()+interval '20 seconds' WHERE id=$1 RETURNING expires_at`, f.claim.ID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	binding, _, err := lifecycleBinding(f.prepared, f.placement)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := f.db.ReserveLifecycle(context.Background(), f.claim, binding)
	if err != nil || !owner.Claim.Until.Equal(expires) {
		t.Fatal("database reservation exceeded request expiry")
	}
}
func TestLifecycleReservationCannotReviveExpiredPreparation(t *testing.T) {
	f := newLifecycleFixture(t)
	var until time.Time
	if err := f.Admin.Pool.QueryRow(`UPDATE execution_requests SET claim_until=clock_timestamp()+interval '700 milliseconds' WHERE id=$1 RETURNING claim_until`, f.claim.ID).Scan(&until); err != nil {
		t.Fatal(err)
	}
	claim := f.claim
	claim.Until = until
	binding, _, err := lifecycleBinding(f.prepared, f.placement)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := f.Admin.Pool.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err = lock.Exec(`SELECT id FROM execution_requests WHERE id=$1 FOR UPDATE`, claim.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.db.ReserveLifecycle(context.Background(), claim, binding); done <- err }()
	f.waitForWorkerLock(t)
	time.Sleep(time.Until(until) + 30*time.Millisecond)
	if err = lock.Commit(); err != nil {
		t.Fatal(err)
	}
	if <-done == nil || f.Count(t, `SELECT count(*) FROM execution_lifecycles`) != 0 {
		t.Fatal("expired preparation revived")
	}
}

func TestLifecycleConcurrentCallerDoesNotWaitOrStopOwner(t *testing.T) {
	f := newLifecycleFixture(t)
	l := f.open(t)
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- l.RunEffect(context.Background(), "config", func(context.Context, EffectInput) (store.LifecycleResult, error) {
			close(started)
			<-release
			return store.LifecycleResult{UID: store.NewID()}, nil
		})
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := time.Now()
	if err := l.RunEffect(ctx, "config", func(context.Context, EffectInput) (store.LifecycleResult, error) {
		t.Error("concurrent operation invoked")
		return store.LifecycleResult{}, nil
	}); err == nil {
		t.Fatal("concurrent caller accepted")
	}
	if time.Since(before) > time.Second {
		t.Fatal("concurrent caller queued behind effect")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal("concurrent loser stopped owner")
	}
}
