package execution

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func TestExecutionFailureObservationIsImmutableAndNotAuthority(t *testing.T) {
	f := newLifecycleFixture(t)
	candidatePrivileges(t, f.preparationFixture.fixture)
	l := f.open(t)
	ctx := context.Background()
	config, pod := store.NewID(), store.NewID()
	container := "containerd://" + strings.Repeat("b", 64)
	if err := f.db.RecordExecutionFailure(ctx, l.durable, pod, container, 10); err == nil {
		t.Fatal("accepted failure before worker start")
	}
	for _, kind := range []string{"config", "pod", "source", "start"} {
		if err := l.RunEffect(ctx, kind, func(_ context.Context, in EffectInput) (store.LifecycleResult, error) {
			r := store.LifecycleResult{UID: pod}
			if kind == "config" {
				r.UID = config
			}
			if kind == "source" {
				r.Digest = workspacebundle.Digest(in.Source)
				r.Bytes = len(in.Source)
			}
			return r, nil
		}); err != nil {
			t.Fatal(kind, err)
		}
	}
	for _, code := range []int32{0, -1, 256} {
		if f.db.RecordExecutionFailure(ctx, l.durable, pod, container, code) == nil {
			t.Fatal("invalid exit accepted", code)
		}
	}
	if f.db.RecordExecutionFailure(ctx, l.durable, store.NewID(), container, 10) == nil {
		t.Fatal("foreign Pod accepted")
	}
	if err := f.db.RecordExecutionFailure(ctx, l.durable, pod, container, 10); err != nil {
		t.Fatal(err)
	}
	var before time.Time
	if err := f.Admin.Pool.QueryRow(`SELECT observed_at FROM execution_failures`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := f.db.RecordExecutionFailure(ctx, l.durable, pod, container, 10); err != nil {
		t.Fatal("exact observation was not idempotent", err)
	}
	if f.db.RecordExecutionFailure(ctx, l.durable, pod, container, 20) == nil || f.db.RecordExecutionFailure(ctx, l.durable, pod, "containerd://"+strings.Repeat("c", 64), 10) == nil {
		t.Fatal("contradictory observation accepted")
	}
	var after time.Time
	if err := f.Admin.Pool.QueryRow(`SELECT observed_at FROM execution_failures`).Scan(&after); err != nil || !before.Equal(after) {
		t.Fatal("observation timestamp renewed")
	}
	for _, statement := range []string{`UPDATE execution_failures SET exit_code=20`, `DELETE FROM execution_failures`, `INSERT INTO execution_failures(request_id,pod_uid,container_id,exit_code,observed_at) SELECT request_id,pod_uid,container_id,exit_code,clock_timestamp() FROM execution_failures WHERE false`} {
		if _, err := f.db.Pool.Exec(statement); err == nil {
			t.Fatal("worker modified immutable failure")
		}
	}
	if err := f.Admin.WithTx(ctx, func(tx *sql.Tx) error {
		if _, e := tx.Exec(`SET LOCAL ROLE aisdlc_intake`); e != nil {
			return e
		}
		_, e := tx.Exec(`INSERT INTO execution_failures(request_id,pod_uid,container_id,exit_code) VALUES($1,$2,$3,10)`, l.durable.Claim.ID, pod, container)
		return e
	}); err == nil {
		t.Fatal("foreign component inserted a diagnostic")
	}
	if err := f.db.StopExecutionClaim(ctx, l.durable.Claim, "uncertain"); err != nil {
		t.Fatal(err)
	}
	if f.db.RecordExecutionFailure(ctx, l.durable, pod, container, 10) == nil {
		t.Fatal("stopped claim retained observation authority")
	}
	if f.Count(t, `SELECT count(*) FROM execution_failures WHERE exit_code=10`) != 1 || f.Count(t, `SELECT count(*) FROM execution_candidates`) != 0 || f.Count(t, `SELECT count(*) FROM execution_effects WHERE kind='export'`) != 0 {
		t.Fatal("failure lost or granted candidate authority")
	}
}

func TestWorkerFailureDiagnosticPreservesCleanupCause(t *testing.T) {
	for _, err := range []error{workerFailure{code: 10, recorded: true}, cleanupFailure{cause: workerFailure{code: 10, recorded: true}}} {
		code, recorded := WorkerFailureDiagnostic(err)
		if DiagnosticStage(err) != "worker-exit" || code != 10 || !recorded {
			t.Fatal("diagnostic lost")
		}
	}
	code, recorded := WorkerFailureDiagnostic(ControllerError{"worker-exit"})
	if code != 0 || recorded {
		t.Fatal("unknown failure fabricated a record")
	}
}
