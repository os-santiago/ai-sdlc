package execution

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func modelLifecycleFixture(t *testing.T, configure ...func(*fixture)) (*lifecycleFixture, *Lifecycle, ModelPolicyLoader, string) {
	t.Helper()
	f := newLifecycleFixture(t, configure...)
	_, err := f.Admin.Pool.Exec(`GRANT SELECT,INSERT ON execution_models,model_budgets,model_allocations TO aisdlc_worker; GRANT SELECT ON execution_candidates TO aisdlc_worker; GRANT UPDATE(status,updated_at) ON work_items TO aisdlc_worker`)
	if err != nil {
		t.Fatal(err)
	}
	l := f.open(t)
	pod := store.NewID()
	for _, kind := range []string{"config", "pod", "source"} {
		if err = l.RunEffect(context.Background(), kind, func(_ context.Context, in EffectInput) (store.LifecycleResult, error) {
			r := store.LifecycleResult{UID: pod}
			if kind == "config" {
				r.UID = store.NewID()
			}
			if kind == "source" {
				r.Digest = workspacebundle.Digest(in.Source)
				r.Bytes = len(in.Source)
			}
			return r, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	load := func(context.Context) (ModelPolicy, error) {
		return ModelPolicy{Limits: store.ModelLimits{Calls: 4, InputBytes: 512 * 1024, OutputBytes: 128 * 1024}, ResponseLimit: 32 * 1024}, nil
	}
	return f, l, load, pod
}
func startModelLifecycle(t *testing.T, l *Lifecycle, pod string) {
	t.Helper()
	if err := l.RunEffect(context.Background(), "start", func(context.Context, EffectInput) (store.LifecycleResult, error) {
		return store.LifecycleResult{UID: pod}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleModelActivationBindsCurrentOwnerAndStart(t *testing.T) {
	f, l, load, pod := modelLifecycleFixture(t)
	scope, err := l.ActivateModel(context.Background(), load)
	if err != nil {
		t.Fatal(err)
	}
	if scope.WorkItem != f.command.WorkItemID || scope.PodUID != pod || scope.PolicyDigest != f.command.ProfileSHA256 || scope.Model != "fixture" || !scope.ExpiresAt.Equal(l.durable.Claim.Until) {
		t.Fatal("scope binding")
	}
	for _, table := range []string{"execution_models", "model_budgets", "model_allocations"} {
		if f.Count(t, "SELECT count(*) FROM "+table) != 1 {
			t.Fatal("missing atomic activation", table)
		}
	}
	if f.Count(t, `SELECT count(*) FROM work_items WHERE status='running'`) != 1 {
		t.Fatal("work state")
	}
	if _, err = l.Authorize(context.Background(), scope); err == nil {
		t.Fatal("model before start acknowledgement")
	}
	if _, _, _, err := preparationProof(context.Background(), f.db, f.load, l.prepared.claim); err == nil {
		t.Fatal("preparation accepted running work")
	}
	startModelLifecycle(t, l, pod)
	until, err := l.Authorize(context.Background(), scope)
	if err != nil || !until.Equal(l.durable.Claim.Until) {
		t.Fatal("current owner denied", err)
	}
	if _, err = l.ActivateModel(context.Background(), load); err == nil {
		t.Fatal("activation adopted")
	}
	if _, err = l.Authorize(context.Background(), scope); err != nil {
		t.Fatal("duplicate local activation stopped winner")
	}
	zero := int32(0)
	if err = l.RunEffect(context.Background(), "export", func(context.Context, EffectInput) (store.LifecycleResult, error) {
		return store.LifecycleResult{UID: pod, Digest: strings.Repeat("d", 64), Bytes: 100, ExitCode: &zero}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = l.Authorize(context.Background(), scope); err == nil {
		t.Fatal("model after export")
	}
}

func TestLifecycleModelRejectsForeignScopeAndRevocation(t *testing.T) {
	for _, which := range []string{"allocation", "work", "pod", "attempt", "source", "policy", "model", "expiry", "response", "generation", "freeze", "budget", "claim", "config"} {
		t.Run(which, func(t *testing.T) {
			f, l, load, pod := modelLifecycleFixture(t)
			scope, err := l.ActivateModel(context.Background(), load)
			if err != nil {
				t.Fatal(err)
			}
			startModelLifecycle(t, l, pod)
			switch which {
			case "allocation":
				scope.AllocationID = store.NewID()
			case "work":
				scope.WorkItem = store.NewID()
			case "pod":
				scope.PodUID = store.NewID()
			case "attempt":
				scope.Attempt++
			case "source":
				scope.SourceSHA = strings.Repeat("d", 40)
			case "policy":
				scope.PolicyDigest = strings.Repeat("d", 64)
			case "model":
				scope.Model = "other"
			case "expiry":
				scope.ExpiresAt = scope.ExpiresAt.Add(time.Second)
			case "response":
				scope.ResponseLimit++
			case "generation":
				_, err = f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=2`)
			case "freeze":
				f.mu.Lock()
				f.inputs.Policy.Freeze = true
				f.mu.Unlock()
			case "budget":
				_, err = f.Admin.Pool.Exec(`UPDATE model_budgets SET state='stopped'`)
			case "claim":
				err = f.db.StopExecutionClaim(context.Background(), l.durable.Claim, "aborted")
			case "config":
				l.model.load = func(context.Context) (ModelPolicy, error) {
					p, _ := load(context.Background())
					p.Limits.Calls++
					return p, nil
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = l.Authorize(context.Background(), scope); err == nil {
				t.Fatal("changed authority accepted")
			}
		})
	}
}

func TestLifecycleModelActivationConflictRollsBackWorkState(t *testing.T) {
	f, l, load, _ := modelLifecycleFixture(t)
	if _, err := f.Admin.Pool.Exec(`INSERT INTO model_budgets(work_item_uuid,repository,source_sha,policy_digest,model,max_calls,max_input_bytes,max_output_bytes,expires_at) VALUES($1,$2,$3,$4,'conflict',1,100,100,clock_timestamp()+interval '1 minute')`, f.command.WorkItemID, f.command.Repository, f.command.SourceSHA, f.command.ProfileSHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ActivateModel(context.Background(), load); err == nil {
		t.Fatal("preexisting budget adopted")
	}
	if f.Count(t, `SELECT count(*) FROM work_items WHERE status='ready'`) != 1 || f.Count(t, `SELECT count(*) FROM execution_models`) != 0 || f.Count(t, `SELECT count(*) FROM model_allocations`) != 0 {
		t.Fatal("partial activation persisted")
	}
	if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='stopped'`) != 1 {
		t.Fatal("uncertain owner active")
	}
}

func TestLifecycleModelConcurrentActivationHasOneOwner(t *testing.T) {
	f, l, load, _ := modelLifecycleFixture(t)
	var wg sync.WaitGroup
	var successes atomic.Int32
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.ActivateModel(context.Background(), load); err == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 || f.Count(t, `SELECT count(*) FROM execution_models`) != 1 || f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='claimed'`) != 1 {
		t.Fatal("concurrent activation ownership")
	}
}

func TestLifecycleModelLostCommitNeverExposesAllocation(t *testing.T) {
	f, l, load, _ := modelLifecycleFixture(t)
	// The first COMMIT is the bounded Pod-identity read. The second is activation.
	faulted, dropped := executionCommitFault(t, 2)
	l.db = faulted
	scope, err := l.ActivateModel(context.Background(), load)
	if err == nil || scope.AllocationID != "" || l.model != nil {
		t.Fatal("uncertain activation exposed capability")
	}
	select {
	case <-dropped:
	default:
		t.Fatal("actual COMMIT ACK not dropped")
	}
	if f.Count(t, `SELECT count(*) FROM execution_models`) != 1 || f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='stopped'`) != 1 {
		t.Fatal("uncertain activation evidence or stop missing")
	}
	l.db = f.db
	if _, err = l.ActivateModel(context.Background(), load); err == nil {
		t.Fatal("uncertain allocation adopted")
	}
}

func TestLifecycleModelRevocationAfterCommitStopsCapability(t *testing.T) {
	f, l, load, _ := modelLifecycleFixture(t)
	revoked := false
	changed := func(ctx context.Context) (ModelPolicy, error) {
		p, err := load(ctx)
		if f.Count(t, `SELECT count(*) FROM execution_models`) > 0 {
			p.Limits.Calls++
			revoked = true
		}
		return p, err
	}
	scope, err := l.ActivateModel(context.Background(), changed)
	if err == nil || !revoked || scope.AllocationID != "" || !l.stopped {
		t.Fatal("post-commit revocation accepted")
	}
	if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='stopped'`) != 1 {
		t.Fatal("owner retained")
	}
}

func TestLifecycleModelExpiryAfterWorkLockRollsBackEverything(t *testing.T) {
	f, l, load, pod := modelLifecycleFixture(t)
	f.shortenLifecycle(t, l)
	p, _ := load(context.Background())
	b := store.ModelBudget{WorkItem: f.command.WorkItemID, Repository: f.command.Repository, SourceSHA: f.command.SourceSHA, PolicyDigest: f.command.ProfileSHA256, Model: "fixture", Limits: p.Limits, ExpiresAt: l.durable.Claim.Until}
	a := store.ModelAllocation{ID: store.NewID(), WorkItem: b.WorkItem, PodUID: pod, Attempt: 1, Limits: b.Limits, ResponseLimit: p.ResponseLimit, ExpiresAt: b.ExpiresAt}
	lock, err := f.Admin.Pool.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err = lock.Exec(`SELECT uuid FROM work_items WHERE uuid=$1 FOR UPDATE`, b.WorkItem); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, e := f.db.ActivateLifecycleModel(context.Background(), l.durable, b, a, strings.Repeat("a", 64))
		done <- e
	}()
	f.waitForWorkerLock(t)
	time.Sleep(time.Until(l.durable.Claim.Until) + 30*time.Millisecond)
	if err = lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	if <-done == nil {
		t.Fatal("expired activation committed")
	}
	if f.Count(t, `SELECT count(*) FROM work_items WHERE status='ready'`) != 1 || f.Count(t, `SELECT count(*) FROM execution_models`) != 0 || f.Count(t, `SELECT count(*) FROM model_budgets`) != 0 || f.Count(t, `SELECT count(*) FROM model_allocations`) != 0 {
		t.Fatal("partial late activation")
	}
}

func TestLifecycleModelReadCannotConcealAuthorityRevocation(t *testing.T) {
	f, l, load, pod := modelLifecycleFixture(t)
	scope, err := l.ActivateModel(context.Background(), load)
	if err != nil {
		t.Fatal(err)
	}
	startModelLifecycle(t, l, pod)
	calls := 0
	l.model.load = func(ctx context.Context) (ModelPolicy, error) {
		calls++
		if calls == 3 {
			f.mu.Lock()
			f.inputs.Policy.Freeze = true
			f.mu.Unlock()
		}
		return load(ctx)
	}
	until, err := l.Authorize(context.Background(), scope)
	if calls < 3 {
		t.Fatal("fault did not trigger")
	}
	if err == nil {
		t.Fatalf("revoked authority accepted after final model read: calls=%d until=%v", calls, until)
	}
}

func TestLifecycleModelRevalidatesRevocationAfterRealLinkageLock(t *testing.T) {
	for _, which := range []string{"freeze", "generation", "model-policy"} {
		t.Run(which, func(t *testing.T) {
			f, l, base, pod := modelLifecycleFixture(t)
			var revoked atomic.Bool
			load := func(ctx context.Context) (ModelPolicy, error) {
				p, e := base(ctx)
				if revoked.Load() {
					p.Limits.Calls++
				}
				return p, e
			}
			scope, err := l.ActivateModel(context.Background(), load)
			if err != nil {
				t.Fatal(err)
			}
			startModelLifecycle(t, l, pod)
			lock, err := f.Admin.Pool.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback()
			if _, err = lock.Exec(`SELECT request_id FROM execution_lifecycles WHERE request_id=$1 FOR UPDATE`, l.durable.Claim.ID); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, e := l.Authorize(context.Background(), scope); done <- e }()
			f.waitForWorkerLock(t)
			switch which {
			case "freeze":
				f.mu.Lock()
				f.inputs.Policy.Freeze = true
				f.mu.Unlock()
			case "generation":
				_, err = f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=2`)
			case "model-policy":
				revoked.Store(true)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = lock.Rollback(); err != nil {
				t.Fatal(err)
			}
			if <-done == nil {
				t.Fatal("revocation hidden by actual linkage row lock")
			}
		})
	}
}

func TestLifecycleModelFixtureCannotRewriteActivationBindings(t *testing.T) {
	f, l, load, _ := modelLifecycleFixture(t)
	if _, err := l.ActivateModel(context.Background(), load); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`UPDATE execution_models SET config_digest=repeat('d',64)`, `DELETE FROM execution_models`, `UPDATE model_budgets SET max_calls=32`, `UPDATE model_allocations SET response_limit=262144`} {
		if _, err := f.db.Pool.Exec(query); err == nil {
			t.Fatal("immutable activation mutation permitted")
		}
	}
}
