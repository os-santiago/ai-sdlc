package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func upgradeFenceFixture(t *testing.T, count int) (*DB, string, []MigrationIdentity) {
	t.Helper()
	catalog, err := MigrationCatalog()
	if err != nil || count > len(catalog) {
		t.Fatal("invalid fixture catalog", err)
	}
	return upgradeFixture(t, len(catalog)-count)
}

func TestUpgradeFencePreservesHistoryAndExcludesWriters(t *testing.T) {
	for _, sourceCount := range []int{33, 34} {
		db, name, source := upgradeFenceFixture(t, sourceCount)
		ctx := context.Background()
		before := upgradeHistory(t, db)
		var retained *UpgradeFence
		err := db.WithPreCandidateUpgradeFence(ctx, name, source, func(ctx context.Context, f *UpgradeFence) error {
			retained = f
			if err := f.Check(ctx); err != nil {
				return err
			}
			var mode string
			if err := f.tx.QueryRowContext(ctx, `SELECT current_setting('transaction_read_only')`).Scan(&mode); err != nil || mode != "off" {
				t.Fatal("administrative SHARE fence mislabeled as read-only", err)
			}
			writer, stop := context.WithTimeout(ctx, 150*time.Millisecond)
			defer stop()
			if _, err := db.Pool.ExecContext(writer, `INSERT INTO public.outbox(id,aggregate_type,aggregate_id,event_type,payload,correlation_id) VALUES($1,'fixture','blocked','fixture','{}','fixture')`, NewID()); err == nil {
				t.Fatal("writer escaped the original fence")
			}
			if upgradeHistory(t, db) != before {
				t.Fatal("fence changed work or outbox")
			}
			return f.Check(ctx)
		})
		if err != nil || upgradeHistory(t, db) != before {
			t.Fatal("administrative fence lost original history", err)
		}
		if retained.Check(ctx) == nil {
			t.Fatal("released fence remained usable")
		}
		if _, err = db.Pool.ExecContext(ctx, `INSERT INTO public.outbox(id,aggregate_type,aggregate_id,event_type,payload,correlation_id) VALUES($1,'fixture','after','fixture','{}','fixture')`, NewID()); err != nil {
			t.Fatal("original producers remained blocked after release", err)
		}
	}
}

func fenceRequest(t *testing.T, db *DB) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	var work string
	if err := db.Pool.QueryRowContext(ctx, `SELECT uuid FROM public.work_items WHERE id='ready'`).Scan(&work); err != nil {
		t.Fatal(err)
	}
	id := "execution-" + strings.Repeat("a", 64)
	token := NewID()
	if _, err := db.Pool.ExecContext(ctx, `INSERT INTO public.execution_requests(id,work_item_uuid,attempt,repository,authority_revision,payload_digest,payload,receipt,expires_at,state,claim_token,claim_until)
  VALUES($1,$2,1,'example/app',$3,$3,'{}','{}',now()+interval '1 hour','claimed',$4,now()+interval '10 minutes')`, id, work, strings.Repeat("b", 64), token); err != nil {
		t.Fatal(err)
	}
	return id, work, token
}

func TestUpgradeFenceRejectsActivityUntilOriginalCleanupCompletes(t *testing.T) {
	db, name, source := upgradeFenceFixture(t, 33)
	ctx := context.Background()
	id, work, token := fenceRequest(t, db)
	calls := 0
	deny := func() {
		t.Helper()
		err := db.WithPreCandidateUpgradeFence(ctx, name, source, func(context.Context, *UpgradeFence) error { calls++; return nil })
		if !errors.Is(err, ErrUpgradeFenceHistory) || calls != 0 {
			t.Fatal("active/unsupported history reached pause callback", err)
		}
	}
	deny() // active claim must finish through its original owner
	uid := NewID()
	if _, err := db.Pool.ExecContext(ctx, `INSERT INTO public.execution_lifecycles(request_id,claim_token,claim_until,binding_digest,source_digest,source_bytes,namespace,pod_name,config_name,phase,config_uid,pod_uid)
  VALUES($1,$2,now()+interval '10 minutes',$3,$3,1,'ai-sdlc-executors',$4,$5,'start-ready',$6,$7)`, id, token, strings.Repeat("c", 64), "executor-"+uid+"-1", "assignment-"+strings.Repeat("d", 32), NewID(), uid); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.ExecContext(ctx, `UPDATE public.execution_requests SET state='stopped',stop_reason='uncertain' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	deny() // cleanup is still pending
	if _, err := db.Pool.ExecContext(ctx, `UPDATE public.execution_lifecycles SET cleanup_state='complete' WHERE request_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.ExecContext(ctx, `INSERT INTO public.execution_effects(request_id,kind,token) VALUES($1,'config',$2)`, id, NewID()); err != nil {
		t.Fatal(err)
	}
	deny() // unknown effect outcome is not adopted
	if _, err := db.Pool.ExecContext(ctx, `UPDATE public.execution_effects SET result='{}',completed_at=clock_timestamp() WHERE request_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.ExecContext(ctx, `INSERT INTO public.model_budgets(work_item_uuid,repository,source_sha,policy_digest,model,max_calls,max_input_bytes,max_output_bytes,expires_at)
  VALUES($1,'example/app',$2,$3,'fixture',1,1024,1024,now()+interval '1 hour')`, work, strings.Repeat("e", 40), strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	deny()
	if _, err := db.Pool.ExecContext(ctx, `UPDATE public.model_budgets SET state='stopped' WHERE work_item_uuid=$1`, work); err != nil {
		t.Fatal(err)
	}
	allocation := NewID()
	if _, err := db.Pool.ExecContext(ctx, `INSERT INTO public.model_allocations(id,work_item_uuid,attempt,pod_uid,max_calls,max_input_bytes,max_output_bytes,response_limit,expires_at)
  VALUES($1,$2,1,$3,1,1024,1024,1024,now()+interval '1 hour')`, allocation, work, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.ExecContext(ctx, `INSERT INTO public.model_calls(allocation_id,sequence,request_sha256,input_bytes,response_limit,expires_at,state)
  VALUES($1,1,$2,1,1024,now()+interval '1 hour','started')`, allocation, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	deny()
	if _, err := db.Pool.ExecContext(ctx, `UPDATE public.model_calls SET state='stopped' WHERE allocation_id=$1`, allocation); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.ExecContext(ctx, `INSERT INTO public.release_gates(repository,pull_request,state,correlation_id) VALUES('example/app',1,'blocked','fixture')`); err != nil {
		t.Fatal(err)
	}
	deny() // downstream history is retained, never erased by the fence
	var count int
	if err := db.Pool.QueryRowContext(ctx, `SELECT count(*) FROM public.release_gates`).Scan(&count); err != nil || count != 1 {
		t.Fatal("unsupported history was changed", err)
	}
	if _, err := db.Pool.ExecContext(ctx, `DELETE FROM public.release_gates`); err != nil {
		t.Fatal(err)
	} // disposable fixture only
	before := upgradeHistory(t, db)
	if err := db.WithPreCandidateUpgradeFence(ctx, name, source, func(ctx context.Context, f *UpgradeFence) error { calls++; return f.Check(ctx) }); err != nil || calls != 1 || upgradeHistory(t, db) != before {
		t.Fatal("contained failure was confused with active work", err)
	}
}

func TestUpgradeFenceExcludesReplicaAndMigrator(t *testing.T) {
	db, name, source := upgradeFenceFixture(t, 33)
	ctx := context.Background()
	before := upgradeHistory(t, db)
	err := db.WithPreCandidateUpgradeFence(ctx, name, source, func(ctx context.Context, f *UpgradeFence) error {
		if e := db.WithPreCandidateUpgradeFence(ctx, name, source, func(context.Context, *UpgradeFence) error { t.Fatal("second maintenance owner entered"); return nil }); !errors.Is(e, ErrUpgradeFence) {
			t.Fatal("second fence was not excluded", e)
		}
		bounded, stop := context.WithTimeout(ctx, 150*time.Millisecond)
		defer stop()
		if e := db.Migrate(bounded); e == nil {
			t.Fatal("migration ran under the producer fence")
		}
		return f.Check(ctx)
	})
	if err != nil || upgradeHistory(t, db) != before {
		t.Fatal("exclusive fence failed", err)
	}
}

func TestUpgradeFenceCancelsAfterBackendLossAndDeadline(t *testing.T) {
	for _, kill := range []bool{true, false} {
		db, name, source := upgradeFenceFixture(t, 33)
		ctx := context.Background()
		before := upgradeHistory(t, db)
		run, stop := context.WithTimeout(ctx, 2*time.Second)
		if !kill {
			stop()
			run, stop = context.WithTimeout(ctx, time.Second)
		}
		entered := false
		err := db.WithPreCandidateUpgradeFence(run, name, source, func(ctx context.Context, f *UpgradeFence) error {
			entered = true
			if kill {
				var pid int
				if e := f.tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); e != nil {
					return e
				}
				if _, e := db.Pool.ExecContext(ctx, `SELECT pg_terminate_backend($1)`, pid); e != nil {
					return e
				}
			}
			<-ctx.Done()
			return ctx.Err()
		})
		stop()
		if err == nil || !entered || upgradeHistory(t, db) != before {
			t.Fatal("lost backend/deadline was accepted", err)
		}
		if err = db.WithPreCandidateUpgradeFence(ctx, name, source, func(ctx context.Context, f *UpgradeFence) error { return f.Check(ctx) }); err != nil {
			t.Fatal("original locks were not released", err)
		}
	}
}

func TestUpgradeFenceSeesCommittedClaimAfterLockWait(t *testing.T) {
	db, name, source := upgradeFenceFixture(t, 33)
	ctx := context.Background()
	holder, err := db.Pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	if _, err = holder.ExecContext(ctx, `LOCK TABLE public.admission_heads IN ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	called := make(chan bool, 1)
	go func() {
		result <- db.WithPreCandidateUpgradeFence(ctx, name, source, func(context.Context, *UpgradeFence) error { called <- true; return nil })
	}()
	deadline := time.Now().Add(750 * time.Millisecond)
	for {
		var waiting bool
		if err = db.Pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock' AND query LIKE 'LOCK TABLE public.admission_heads%')`, name).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fence did not reach the original table lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// This commits while the maintenance transaction is waiting, after its
	// advisory-lock SELECT. The subsequent READ COMMITTED check must see it.
	fenceRequest(t, db)
	if err = holder.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		if !errors.Is(err, ErrUpgradeFence) {
			t.Fatal("new active claim was missed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fence did not release after active work")
	}
	select {
	case <-called:
		t.Fatal("pause callback observed a stale snapshot")
	default:
	}
}

func TestUpgradeFenceRejectsUnknownSchemaAndSourceWithoutEffects(t *testing.T) {
	db, name, source := upgradeFenceFixture(t, 33)
	ctx := context.Background()
	before := upgradeHistory(t, db)
	callback := func(context.Context, *UpgradeFence) error {
		t.Fatal("unreviewed database reached administrative callback")
		return nil
	}
	if err := db.WithPreCandidateUpgradeFence(ctx, name+"_wrong", source, callback); !errors.Is(err, ErrUpgradeFence) {
		t.Fatal(err)
	}
	if err := db.WithPreCandidateUpgradeFence(ctx, name, source[:32], callback); !errors.Is(err, ErrUpgradeFence) {
		t.Fatal(err)
	}
	if _, err := db.Pool.ExecContext(ctx, `CREATE SCHEMA archived; CREATE TABLE archived.unreviewed(id integer)`); err != nil {
		t.Fatal(err)
	}
	if err := db.WithPreCandidateUpgradeFence(ctx, name, source, callback); !errors.Is(err, ErrUpgradeFence) {
		t.Fatal(err)
	}
	if upgradeHistory(t, db) != before {
		t.Fatal("rejected fence changed history")
	}
}

func TestUpgradeFenceRejectsWrongDatabaseBeforeTableLocks(t *testing.T) {
	db, name, source := upgradeFenceFixture(t, 33)
	ctx := context.Background()
	holder, err := db.Pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	if _, err = holder.ExecContext(ctx, `LOCK TABLE public.admission_heads IN ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = db.WithPreCandidateUpgradeFence(ctx, name+"_wrong", source, func(context.Context, *UpgradeFence) error { t.Fatal("wrong database entered callback"); return nil })
	if !errors.Is(err, ErrUpgradeFence) || time.Since(started) > 750*time.Millisecond {
		t.Fatal("wrong database waited on an administrative lock", err)
	}
}

func TestUpgradeFenceConcurrentHeartbeatChecks(t *testing.T) {
	db, name, source := upgradeFenceFixture(t, 33)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := db.WithPreCandidateUpgradeFence(ctx, name, source, func(active context.Context, fence *UpgradeFence) error {
		var group sync.WaitGroup
		failures := make(chan error, 8)
		start := make(chan struct{})
		for n := 0; n < 8; n++ {
			group.Add(1)
			go func() {
				defer group.Done()
				<-start
				for i := 0; i < 100; i++ {
					if err := fence.Check(active); err != nil {
						failures <- err
						return
					}
					time.Sleep(3 * time.Millisecond)
				}
			}()
		}
		close(start)
		group.Wait()
		close(failures)
		for err := range failures {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal("concurrent checks lost the original administrative fence", err)
	}
}

func TestUpgradeFenceCheckQueueHonorsCancellation(t *testing.T) {
	db, name, source := upgradeFenceFixture(t, 33)
	err := db.WithPreCandidateUpgradeFence(context.Background(), name, source, func(active context.Context, fence *UpgradeFence) error {
		// Hold the operation permit to model another in-flight observation. A
		// cancelled caller must not wait for it or make a new database query.
		fence.checkPermit <- struct{}{}
		queued, cancel := context.WithCancel(active)
		cancel()
		done := make(chan error, 1)
		go func() { done <- fence.Check(queued) }()
		select {
		case err := <-done:
			if !errors.Is(err, ErrUpgradeFence) {
				<-fence.checkPermit
				return errors.New("cancelled queued check did not fail closed")
			}
		case <-time.After(500 * time.Millisecond):
			<-fence.checkPermit
			return errors.New("cancelled queued check waited for the original permit")
		}
		<-fence.checkPermit
		return fence.Check(active)
	})
	if err != nil {
		t.Fatal("check cancellation damaged the original fence", err)
	}
}
