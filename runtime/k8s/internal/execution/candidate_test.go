package execution

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func candidateFixture(t *testing.T, configure ...func(*fixture)) (*lifecycleFixture, *Lifecycle, store.ModelScope, []byte) {
	t.Helper()
	f, l, load, pod := modelLifecycleFixture(t, configure...)
	if _, err := f.Admin.Pool.Exec(`GRANT INSERT ON execution_candidates TO aisdlc_worker; GRANT SELECT ON model_calls TO aisdlc_worker; GRANT UPDATE(state,stop_reason) ON model_budgets TO aisdlc_worker; GRANT UPDATE(reserved_calls) ON model_allocations TO aisdlc_worker`); err != nil {
		t.Fatal(err)
	}
	scope, err := l.ActivateModel(context.Background(), load)
	if err != nil {
		t.Fatal(err)
	}
	startModelLifecycle(t, l, pod)
	if _, err = f.Admin.Pool.Exec(`INSERT INTO model_calls(allocation_id,sequence,request_sha256,input_bytes,response_limit,expires_at,state,output_bytes,response_sha256,response_status,response_handed_off_at)
 VALUES($1,1,repeat('a',64),2,32,$2,'completed',2,repeat('b',64),200,clock_timestamp())`, scope.AllocationID, scope.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Admin.Pool.Exec(`UPDATE model_budgets SET reserved_calls=1,reserved_input_bytes=2,reserved_output_bytes=32`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Admin.Pool.Exec(`UPDATE model_allocations SET reserved_calls=1,reserved_input_bytes=2,reserved_output_bytes=32`); err != nil {
		t.Fatal(err)
	}
	bundle, err := workspacebundle.Decode(l.prepared.Source())
	if err != nil {
		t.Fatal(err)
	}
	if f.candidateChanges == nil {
		bundle.Files = append(bundle.Files, workspacebundle.File{Path: "candidate-fixture.txt", Mode: 0644, Data: []byte("unvalidated\n")})
	} else {
		for name, content := range f.candidateChanges {
			found := false
			for i := range bundle.Files {
				if bundle.Files[i].Path == name {
					bundle.Files[i].Data = []byte(content)
					found = true
				}
			}
			if !found {
				bundle.Files = append(bundle.Files, workspacebundle.File{Path: name, Mode: 0644, Data: []byte(content)})
			}
		}
	}
	raw, err := workspacebundle.Encode(bundle)
	if err != nil {
		t.Fatal(err)
	}
	zero := int32(0)
	if err = l.RunEffect(context.Background(), "export", func(context.Context, EffectInput) (store.LifecycleResult, error) {
		return store.LifecycleResult{UID: pod, Digest: workspacebundle.Digest(raw), Bytes: len(raw), ExitCode: &zero}, nil
	}); err != nil {
		t.Fatal(err)
	}
	return f, l, scope, raw
}

func TestCandidateCapturePersistsExactDeltaAndSealsBudget(t *testing.T) {
	f, l, scope, raw := candidateFixture(t)
	record, err := l.CaptureCandidate(context.Background(), raw)
	if err != nil || record.Changes != 1 || record.ModelCalls != 1 || record.ExportSHA256 != workspacebundle.Digest(raw) {
		t.Fatal("capture", err)
	}
	var deltaRaw []byte
	if err = f.Admin.Pool.QueryRow(`SELECT delta FROM execution_candidates WHERE request_id=$1`, record.RequestID).Scan(&deltaRaw); err != nil {
		t.Fatal(err)
	}
	delta, err := candidate.Decode(deltaRaw)
	if err != nil {
		t.Fatal(err)
	}
	source, _ := workspacebundle.Decode(l.prepared.Source())
	rebuilt, err := candidate.Apply(source, delta)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := workspacebundle.Decode(raw)
	wantTree, _ := want.TreeDigest()
	tree, _ := rebuilt.TreeDigest()
	if wantTree != tree || record.DeltaSHA256 != workspacebundle.Digest(deltaRaw) {
		t.Fatal("immutable delta differs")
	}
	if f.Count(t, `SELECT count(*) FROM model_budgets WHERE state='stopped' AND stop_reason='candidate-captured' AND reserved_calls=1`) != 1 || f.Count(t, `SELECT count(*) FROM execution_lifecycles WHERE phase='candidate-captured'`) != 1 || f.Count(t, `SELECT count(*) FROM work_items WHERE status='running'`) != 1 {
		t.Fatal("capture state")
	}
	if _, err = l.Authorize(context.Background(), scope); err == nil {
		t.Fatal("captured model authority")
	}
	if _, err = l.CaptureCandidate(context.Background(), raw); err == nil {
		t.Fatal("capture replay adopted")
	}
	if _, err = f.db.ReadClaimedExecution(context.Background(), l.durable.Claim); err != nil {
		t.Fatal("duplicate stopped original claim", err)
	}
	for _, query := range []string{`UPDATE execution_candidates SET changes=2`, `DELETE FROM execution_candidates`} {
		if _, err = f.db.Pool.Exec(query); err == nil {
			t.Fatal("candidate mutable")
		}
	}
	if _, phase, err := l.proofPhase(context.Background()); err != nil || phase != "candidate-captured" {
		t.Fatal("sealed model read", err)
	}
	if _, err = f.Admin.Pool.Exec(`DELETE FROM execution_candidates`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = l.proofPhase(context.Background()); err == nil {
		t.Fatal("sealed model without matching candidate")
	}
}

func TestCandidateCaptureRejectsUntrustedOrRevokedEvidence(t *testing.T) {
	for _, which := range []string{"bytes", "source", "exit", "pod", "export-size", "missing-export", "empty", "pending", "ambiguous", "no-handoff", "no-calls", "stopped-budget", "generation", "freeze", "model-policy", "missing-model"} {
		t.Run(which, func(t *testing.T) {
			f, l, _, raw := candidateFixture(t)
			var err error
			switch which {
			case "bytes":
				raw = append(raw, ' ')
			case "source":
				b, _ := workspacebundle.Decode(raw)
				b.SourceSHA = strings.Repeat("b", 40)
				raw, _ = workspacebundle.Encode(b)
			case "empty":
				raw = l.prepared.Source()
			case "exit", "pod", "export-size":
				var value []byte
				err = f.Admin.Pool.QueryRow(`SELECT result FROM execution_effects WHERE kind='export'`).Scan(&value)
				if err != nil {
					t.Fatal(err)
				}
				var receipt store.LifecycleResult
				if json.Unmarshal(value, &receipt) != nil {
					t.Fatal("receipt")
				}
				if which == "exit" {
					code := int32(1)
					receipt.ExitCode = &code
				}
				if which == "pod" {
					receipt.UID = store.NewID()
				}
				if which == "export-size" {
					receipt.Bytes++
				}
				value, _ = json.Marshal(receipt)
				_, err = f.Admin.Pool.Exec(`UPDATE execution_effects SET result=$1 WHERE kind='export'`, value)
			case "missing-export":
				_, err = f.Admin.Pool.Exec(`DELETE FROM execution_effects WHERE kind='export'`)
			case "pending":
				_, err = f.Admin.Pool.Exec(`UPDATE model_calls SET state='started',output_bytes=NULL,response_sha256=NULL,response_status=NULL,response_handed_off_at=NULL`)
			case "ambiguous":
				_, err = f.Admin.Pool.Exec(`UPDATE model_calls SET state='ambiguous',response_handed_off_at=NULL`)
			case "no-handoff":
				_, err = f.Admin.Pool.Exec(`UPDATE model_calls SET response_handed_off_at=NULL`)
			case "no-calls":
				_, err = f.Admin.Pool.Exec(`DELETE FROM model_calls`)
			case "stopped-budget":
				_, err = f.Admin.Pool.Exec(`UPDATE model_budgets SET state='stopped',stop_reason='uncertain'`)
			case "generation":
				_, err = f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=2`)
			case "freeze":
				f.mu.Lock()
				f.inputs.Policy.Freeze = true
				f.mu.Unlock()
			case "model-policy":
				l.model.load = func(context.Context) (ModelPolicy, error) { return ModelPolicy{}, nil }
			case "missing-model":
				_, err = f.Admin.Pool.Exec(`DELETE FROM execution_models`)
			}
			if err != nil {
				t.Fatal(err)
			}
			if record, err := l.CaptureCandidate(context.Background(), raw); err == nil || record.RequestID != "" {
				t.Fatal("invalid capture returned evidence")
			}
			if f.Count(t, `SELECT count(*) FROM execution_candidates`) != 0 {
				t.Fatal("invalid capture persisted")
			}
		})
	}
}

func TestCandidateConcurrentCaptureHasOneWinner(t *testing.T) {
	f, l, _, raw := candidateFixture(t)
	var wg sync.WaitGroup
	var winners atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.CaptureCandidate(context.Background(), raw); err == nil {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 || f.Count(t, `SELECT count(*) FROM execution_candidates`) != 1 || f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='claimed'`) != 1 {
		t.Fatal("capture ownership")
	}
}

func TestCandidateLostCommitDoesNotExposeOrAdoptCapture(t *testing.T) {
	f, l, _, raw := candidateFixture(t)
	// Two current model reads commit before the candidate transaction.
	faulted, dropped := executionCommitFault(t, 3)
	l.db = faulted
	if record, err := l.CaptureCandidate(context.Background(), raw); err == nil || record.RequestID != "" {
		t.Fatal("lost ACK exposed candidate")
	}
	select {
	case <-dropped:
	default:
		t.Fatal("candidate COMMIT ACK was not dropped")
	}
	if f.Count(t, `SELECT count(*) FROM execution_candidates`) != 1 || f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='stopped'`) != 1 || f.Count(t, `SELECT count(*) FROM model_budgets WHERE state='stopped' AND stop_reason='candidate-captured'`) != 1 {
		t.Fatal("uncertain evidence/stop")
	}
	l.db = f.db
	if _, err := l.CaptureCandidate(context.Background(), raw); err == nil {
		t.Fatal("uncertain capture adopted")
	}
}

func TestCandidatePostCommitRevocationLeavesNoExposedRecord(t *testing.T) {
	f, l, _, raw := candidateFixture(t)
	load := l.model.load
	l.model.load = func(ctx context.Context) (ModelPolicy, error) {
		p, err := load(ctx)
		if f.Count(t, `SELECT count(*) FROM execution_candidates`) > 0 {
			f.mu.Lock()
			f.inputs.Policy.Freeze = true
			f.mu.Unlock()
		}
		return p, err
	}
	if record, err := l.CaptureCandidate(context.Background(), raw); err == nil || record.RequestID != "" {
		t.Fatal("post-commit revocation accepted")
	}
	if f.Count(t, `SELECT count(*) FROM execution_candidates`) != 1 || f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='stopped'`) != 1 {
		t.Fatal("revoked evidence or claim stop missing")
	}
}

func candidateDelta(t *testing.T, l *Lifecycle, raw []byte) []byte {
	t.Helper()
	source, _ := workspacebundle.Decode(l.prepared.Source())
	result, _ := workspacebundle.Decode(raw)
	d, err := candidate.Build(source, result)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := candidate.Encode(d)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestCandidateExpiryAfterEveryPersistenceLock(t *testing.T) {
	for _, which := range []string{"request", "lifecycle", "effect", "budget"} {
		t.Run(which, func(t *testing.T) {
			f, l, scope, raw := candidateFixture(t)
			delta := candidateDelta(t, l, raw)
			f.shortenLifecycle(t, l)
			lock, err := f.Admin.Pool.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback()
			query := map[string]string{"request": `SELECT id FROM execution_requests FOR UPDATE`, "lifecycle": `SELECT request_id FROM execution_lifecycles FOR UPDATE`, "effect": `SELECT request_id FROM execution_effects WHERE kind='export' FOR UPDATE`, "budget": `SELECT work_item_uuid FROM model_budgets FOR UPDATE`}[which]
			if _, err = lock.Exec(query); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, e := f.db.CaptureLifecycleCandidate(context.Background(), l.durable, scope.AllocationID, raw, delta)
				done <- e
			}()
			f.waitForWorkerLock(t)
			time.Sleep(time.Until(l.durable.Claim.Until) + 30*time.Millisecond)
			if err = lock.Commit(); err != nil {
				t.Fatal(err)
			}
			if <-done == nil || f.Count(t, `SELECT count(*) FROM execution_candidates`) != 0 || f.Count(t, `SELECT count(*) FROM model_budgets WHERE state='active'`) != 1 {
				t.Fatal("expired capture committed")
			}
		})
	}
}

func TestCandidateAndModelReservationSerializeAtBudget(t *testing.T) {
	for _, first := range []string{"capture", "reserve"} {
		t.Run(first, func(t *testing.T) {
			f, l, scope, raw := candidateFixture(t)
			delta := candidateDelta(t, l, raw)
			if _, err := f.Admin.Pool.Exec(`GRANT SELECT,INSERT,UPDATE ON model_calls TO aisdlc_worker; GRANT UPDATE(reserved_calls,reserved_input_bytes,reserved_output_bytes) ON model_budgets,model_allocations TO aisdlc_worker`); err != nil {
				t.Fatal(err)
			}
			lock, err := f.Admin.Pool.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback()
			if _, err = lock.Exec(`SELECT work_item_uuid FROM model_budgets FOR UPDATE`); err != nil {
				t.Fatal(err)
			}
			captured := make(chan error, 1)
			reserved := make(chan store.ModelDecision, 1)
			reserveErr := make(chan error, 1)
			capture := func() {
				_, e := f.db.CaptureLifecycleCandidate(context.Background(), l.durable, scope.AllocationID, raw, delta)
				captured <- e
			}
			reserve := func() {
				d, e := f.db.ReserveModelCall(context.Background(), scope.AllocationID, scope.PodUID, 2, []byte(`{"model":"fixture","stream":false,"messages":[{"role":"user","content":"fixture"}]}`))
				reserved <- d
				reserveErr <- e
			}
			if first == "capture" {
				go capture()
				f.waitForWorkerLock(t)
				go reserve()
			} else {
				go reserve()
				f.waitForWorkerLock(t)
				go capture()
			}
			deadline := time.Now().Add(2 * time.Second)
			for f.Count(t, `SELECT count(*) FROM pg_stat_activity WHERE usename='aisdlc_worker' AND wait_event_type='Lock'`) < 2 {
				if time.Now().After(deadline) {
					t.Fatal("second budget waiter absent")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err = lock.Commit(); err != nil {
				t.Fatal(err)
			}
			captureErr := <-captured
			decision := <-reserved
			if err = <-reserveErr; err != nil {
				t.Fatal(err)
			}
			count := f.Count(t, `SELECT count(*) FROM execution_candidates`)
			if captureErr == nil {
				if count != 1 || decision.Reason != "inactive" || f.Count(t, `SELECT count(*) FROM model_calls`) != 1 {
					t.Fatal("reservation passed sealed capture", decision.Reason)
				}
			} else {
				if count != 0 || decision.Reason != "reserved" || f.Count(t, `SELECT count(*) FROM model_calls`) != 2 {
					t.Fatal("pending call did not block capture", decision.Reason)
				}
			}
		})
	}
}
