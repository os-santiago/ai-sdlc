package store

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

var modelTestPayload = []byte(`{"model":"fixture-model","messages":[{"role":"user","content":"private test input"}],"stream":false}`)

func modelFixture(t *testing.T, calls int) (*DB, ModelBudget, ModelAllocation) {
	t.Helper()
	db := testDB(t)
	ctx := context.Background()
	plan, work := NewID(), NewID()
	if err := UpsertPlan(ctx, db.Pool, PlanRecord{ID: plan, Repository: "fixture/product", CorrelationID: NewID()}); err != nil {
		t.Fatal(err)
	}
	if err := UpsertWorkItem(ctx, db.Pool, WorkItemRecord{PlanID: plan, ID: "fixture", UUID: work, Status: "running", IssueNumbers: []int{1}, AcceptanceCriteria: []string{"fixture"}}); err != nil {
		t.Fatal(err)
	}
	b := ModelBudget{WorkItem: work, Repository: "fixture/product", SourceSHA: strings.Repeat("a", 40), PolicyDigest: strings.Repeat("b", 64), Model: "fixture-model", Limits: ModelLimits{calls, 100000, 100000}, ExpiresAt: time.Now().Add(30 * time.Minute)}
	if err := db.CreateModelBudget(ctx, b); err != nil {
		t.Fatal(err)
	}
	a := ModelAllocation{ID: NewID(), WorkItem: work, PodUID: NewID(), Attempt: 1, Limits: ModelLimits{calls, 10000, 10000}, ResponseLimit: 1024, ExpiresAt: time.Now().Add(20 * time.Minute)}
	if err := db.CreateModelAllocation(ctx, a); err != nil {
		t.Fatal(err)
	}
	return db, b, a
}
func modelReserve(t *testing.T, db *DB, a ModelAllocation, seq int) ModelCallRef {
	t.Helper()
	d, err := db.ReserveModelCall(context.Background(), a.ID, a.PodUID, seq, modelTestPayload)
	if err != nil || d.Reason != "reserved" {
		t.Fatalf("reserve: %+v %v", d, err)
	}
	return d.Call.ModelCallRef
}
func modelCount(t *testing.T, db *DB, work string) (int, int64, int64) {
	t.Helper()
	var c int
	var i, o int64
	if err := db.Pool.QueryRow(`SELECT reserved_calls,reserved_input_bytes,reserved_output_bytes FROM model_budgets WHERE work_item_uuid=$1`, work).Scan(&c, &i, &o); err != nil {
		t.Fatal(err)
	}
	return c, i, o
}
func TestModelReservationConcurrentIdempotenceAndSingleClaim(t *testing.T) {
	db, b, a := modelFixture(t, 4)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan ModelDecision, 20)
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, e := db.ReserveModelCall(ctx, a.ID, a.PodUID, 1, modelTestPayload)
			results <- d
			errs <- e
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	reserved := 0
	var ref ModelCallRef
	for d := range results {
		if d.Reason == "reserved" {
			reserved++
			ref = d.Call.ModelCallRef
		} else if d.Reason != "existing" {
			t.Errorf("unexpected %+v", d)
		}
	}
	for e := range errs {
		if e != nil {
			t.Error(e)
		}
	}
	if reserved != 1 {
		t.Fatalf("reservations=%d", reserved)
	}
	c, i, o := modelCount(t, db, b.WorkItem)
	if c != 1 || i != int64(len(modelTestPayload)) || o != 1024 {
		t.Fatalf("counters %d %d %d", c, i, o)
	}
	results = make(chan ModelDecision, 20)
	errs = make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); d, e := db.ClaimModelCall(ctx, ref); results <- d; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	invocations := 0
	for d := range results {
		if d.Invoke {
			invocations++
		} else if d.Reason != "already-claimed" {
			t.Errorf("claim: %+v", d)
		}
	}
	for e := range errs {
		if e != nil {
			t.Error(e)
		}
	}
	if invocations != 1 {
		t.Fatalf("invocations=%d", invocations)
	}
	// A new connection/process cannot turn an observed started state into a retry.
	other, err := Open(ctx, os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	d, err := other.ClaimModelCall(ctx, ref)
	if err != nil || d.Invoke || d.Reason != "already-claimed" {
		t.Fatalf("restart claim %+v %v", d, err)
	}
	d, err = other.FinishModelCall(ctx, ref, 200, []byte(`{"ok":true}`))
	if err != nil || d.Reason != "completed" {
		t.Fatalf("finish %+v %v", d, err)
	}
	d, err = other.FinishModelCall(ctx, ref, 200, []byte(`{"ok":true}`))
	if err != nil || d.Reason != "existing" {
		t.Fatalf("repeat finish %+v %v", d, err)
	}
	_, _, o = modelCount(t, db, b.WorkItem)
	if o != 1024 {
		t.Fatal("completion refunded reserved output")
	}
	d, err = other.ClaimModelCall(ctx, ref)
	if err != nil || d.Invoke {
		t.Fatalf("completed claim %+v %v", d, err)
	}
}
func TestModelBudgetSharedAcrossAttempts(t *testing.T) {
	db, b, a := modelFixture(t, 1)
	other := a
	other.ID = NewID()
	other.PodUID = NewID()
	other.Attempt = 2
	if err := db.CreateModelAllocation(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	results := make(chan ModelDecision, 2)
	errs := make(chan error, 2)
	for _, allocation := range []ModelAllocation{a, other} {
		go func(a ModelAllocation) {
			d, e := db.ReserveModelCall(context.Background(), a.ID, a.PodUID, 1, modelTestPayload)
			results <- d
			errs <- e
		}(allocation)
	}
	reserved, exhausted := 0, 0
	for range 2 {
		d := <-results
		if d.Reason == "reserved" {
			reserved++
		}
		if d.Reason == "budget-exhausted" {
			exhausted++
		}
		if e := <-errs; e != nil {
			t.Fatal(e)
		}
	}
	if reserved != 1 || exhausted != 1 {
		t.Fatalf("reserved=%d exhausted=%d", reserved, exhausted)
	}
	c, _, _ := modelCount(t, db, b.WorkItem)
	if c != 1 {
		t.Fatal(c)
	}
	if err := db.CreateModelBudget(context.Background(), b); err == nil {
		t.Fatal("budget reset accepted")
	}
	replacement := a
	replacement.ID = NewID()
	replacement.PodUID = NewID()
	if err := db.CreateModelAllocation(context.Background(), replacement); err == nil {
		t.Fatal("replacement pod accepted for same attempt")
	}
}
func TestModelReservationBindingsFailClosed(t *testing.T) {
	for _, kind := range []string{"changed-body", "wrong-pod", "wrong-model", "skipped-sequence", "completion-mismatch"} {
		t.Run(kind, func(t *testing.T) {
			db, b, a := modelFixture(t, 3)
			ctx := context.Background()
			ref := modelReserve(t, db, a, 1)
			var d ModelDecision
			var err error
			switch kind {
			case "changed-body":
				d, err = db.ReserveModelCall(ctx, a.ID, a.PodUID, 1, []byte(`{"model":"fixture-model","messages":[{"role":"user","content":"different"}]}`))
			case "wrong-pod":
				d, err = db.ReserveModelCall(ctx, a.ID, NewID(), 1, modelTestPayload)
			case "wrong-model":
				d, err = db.ReserveModelCall(ctx, a.ID, a.PodUID, 1, []byte(`{"model":"another-model","messages":[{}]}`))
			case "skipped-sequence":
				d, err = db.ReserveModelCall(ctx, a.ID, a.PodUID, 3, modelTestPayload)
			case "completion-mismatch":
				if _, err = db.ClaimModelCall(ctx, ref); err != nil {
					t.Fatal(err)
				}
				if _, err = db.FinishModelCall(ctx, ref, 200, []byte(`{}`)); err != nil {
					t.Fatal(err)
				}
				d, err = db.FinishModelCall(ctx, ref, 200, []byte(`{"different":true}`))
			}
			if err != nil || d.Invoke {
				t.Fatalf("decision %+v %v", d, err)
			}
			var state string
			if err = db.Pool.QueryRow(`SELECT state FROM model_budgets WHERE work_item_uuid=$1`, b.WorkItem).Scan(&state); err != nil || state != "stopped" {
				t.Fatalf("state=%s %v", state, err)
			}
			d, err = db.ClaimModelCall(ctx, ref)
			if err != nil || d.Invoke {
				t.Fatalf("stopped claim %+v %v", d, err)
			}
		})
	}
}
func TestModelAmbiguousNeverRetriesOrRefunds(t *testing.T) {
	db, b, a := modelFixture(t, 3)
	ctx := context.Background()
	ref := modelReserve(t, db, a, 1)
	d, err := db.ClaimModelCall(ctx, ref)
	if err != nil || !d.Invoke {
		t.Fatalf("claim %+v %v", d, err)
	}
	d, err = db.StopModelCall(ctx, ref)
	if err != nil || d.Reason != "ambiguous" {
		t.Fatalf("stop %+v %v", d, err)
	}
	d, err = db.FinishModelCall(ctx, ref, 200, []byte(`{}`))
	if err != nil || d.Reason != "not-started" || d.Call.State != "ambiguous" {
		t.Fatalf("late finish %+v %v", d, err)
	}
	d, err = db.ClaimModelCall(ctx, ref)
	if err != nil || d.Invoke {
		t.Fatalf("claim %+v %v", d, err)
	}
	c, i, o := modelCount(t, db, b.WorkItem)
	if c != 1 || i != int64(len(modelTestPayload)) || o != 1024 {
		t.Fatalf("refunded %d %d %d", c, i, o)
	}
	a.ID = NewID()
	a.PodUID = NewID()
	a.Attempt = 2
	if err = db.CreateModelAllocation(ctx, a); err == nil {
		t.Fatal("new attempt bypassed stopped parent")
	}
}
func TestModelExpirationAfterLockWait(t *testing.T) {
	db, _, a := modelFixture(t, 2)
	ctx := context.Background()
	ref := modelReserve(t, db, a, 1)
	// Fixture DDL authority shortens a deadline; production API cannot do this.
	if _, err := db.Pool.Exec(`UPDATE model_calls SET expires_at=clock_timestamp()+interval '250 milliseconds' WHERE allocation_id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`SELECT 1 FROM model_budgets WHERE work_item_uuid=$1 FOR UPDATE`, a.WorkItem); err != nil {
		t.Fatal(err)
	}
	result := make(chan ModelDecision, 1)
	errs := make(chan error, 1)
	go func() { d, e := db.ClaimModelCall(ctx, ref); result <- d; errs <- e }()
	time.Sleep(400 * time.Millisecond)
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	d := <-result
	if e := <-errs; e != nil || d.Invoke || d.Reason != "inactive" {
		t.Fatalf("expired while waiting %+v %v", d, e)
	}
	n, err := db.RecoverExpiredModelCalls(ctx)
	if err != nil || n < 1 {
		t.Fatalf("recovery %d %v", n, err)
	}
	d, err = db.ClaimModelCall(ctx, ref)
	if err != nil || d.Invoke {
		t.Fatalf("after recovery %+v %v", d, err)
	}
}
func TestModelCompletionRecoveryRace(t *testing.T) {
	for range 10 {
		db, _, a := modelFixture(t, 2)
		ctx := context.Background()
		ref := modelReserve(t, db, a, 1)
		if _, err := db.ClaimModelCall(ctx, ref); err != nil {
			t.Fatal(err)
		}
		errs := make(chan error, 2)
		go func() { _, e := db.StopModelCall(ctx, ref); errs <- e }()
		go func() { _, e := db.FinishModelCall(ctx, ref, 200, []byte(`{}`)); errs <- e }()
		for range 2 {
			if e := <-errs; e != nil {
				t.Fatal(e)
			}
		}
		var state string
		if err := db.Pool.QueryRow(`SELECT state FROM model_calls WHERE allocation_id=$1`, a.ID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "completed" && state != "ambiguous" {
			t.Fatal(state)
		}
		d, e := db.ClaimModelCall(ctx, ref)
		if e != nil || d.Invoke {
			t.Fatalf("terminal restart %+v %v", d, e)
		}
	}
}
func TestModelInvalidRequestsAndPending(t *testing.T) {
	db, _, a := modelFixture(t, 3)
	ctx := context.Background()
	for _, body := range [][]byte{nil, []byte(`{"model":"fixture-model","model":"other","messages":[{}]}`), []byte(`{"model":"fixture-model","messages":[{}],"provider_url":"https://other"}`), []byte(`{"model":"fixture-model","messages":[{}],"stream":true}`), []byte(strings.Repeat("x", 131073))} {
		if _, err := db.ReserveModelCall(ctx, a.ID, a.PodUID, 1, body); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
	modelReserve(t, db, a, 1)
	d, e := db.ReserveModelCall(ctx, a.ID, a.PodUID, 2, modelTestPayload)
	if e != nil || d.Reason != "pending" {
		t.Fatalf("pending %+v %v", d, e)
	}
}
func TestModelByteLimitsAndResponseOverflow(t *testing.T) {
	db, b, a := modelFixture(t, 3)
	ctx := context.Background()
	if _, err := db.Pool.Exec(`UPDATE model_budgets SET max_input_bytes=$2 WHERE work_item_uuid=$1`, b.WorkItem, len(modelTestPayload)-1); err != nil {
		t.Fatal(err)
	}
	d, e := db.ReserveModelCall(ctx, a.ID, a.PodUID, 1, modelTestPayload)
	if e != nil || d.Reason != "budget-exhausted" {
		t.Fatalf("input budget %+v %v", d, e)
	}
	if _, err := db.Pool.Exec(`UPDATE model_budgets SET max_input_bytes=100000,max_output_bytes=1023 WHERE work_item_uuid=$1`, b.WorkItem); err != nil {
		t.Fatal(err)
	}
	d, e = db.ReserveModelCall(ctx, a.ID, a.PodUID, 1, modelTestPayload)
	if e != nil || d.Reason != "budget-exhausted" {
		t.Fatalf("output budget %+v %v", d, e)
	}
	if _, err := db.Pool.Exec(`UPDATE model_budgets SET max_output_bytes=100000 WHERE work_item_uuid=$1`, b.WorkItem); err != nil {
		t.Fatal(err)
	}
	ref := modelReserve(t, db, a, 1)
	if _, e = db.ClaimModelCall(ctx, ref); e != nil {
		t.Fatal(e)
	}
	d, e = db.FinishModelCall(ctx, ref, 200, []byte(strings.Repeat("x", 1025)))
	if e != nil || d.Reason != "uncertain-outcome" || d.Call.State != "ambiguous" {
		t.Fatalf("overflow %+v %v", d, e)
	}
}
func TestModelCancelledClaimNeverGrantsInvocation(t *testing.T) {
	db, _, a := modelFixture(t, 1)
	ref := modelReserve(t, db, a, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, e := db.ClaimModelCall(ctx, ref)
	if e == nil || d.Invoke {
		t.Fatalf("cancelled %+v %v", d, e)
	}
}
func TestModelPolicyValidation(t *testing.T) {
	db, b, a := modelFixture(t, 2)
	ctx := context.Background()
	for _, mutate := range []func(*ModelAllocation){func(a *ModelAllocation) { a.Limits.Calls = 3 }, func(a *ModelAllocation) { a.ResponseLimit = 262145 }, func(a *ModelAllocation) { a.ExpiresAt = time.Now().Add(-time.Second) }, func(a *ModelAllocation) { a.ExpiresAt = time.Now().Add(2 * time.Hour) }} {
		candidate := a
		candidate.ID = NewID()
		candidate.PodUID = NewID()
		candidate.Attempt = 2
		mutate(&candidate)
		if err := db.CreateModelAllocation(ctx, candidate); err == nil {
			t.Fatal("invalid allocation")
		}
	}
	b.Model = ""
	if err := db.CreateModelBudget(ctx, b); err == nil {
		t.Fatal("empty model")
	}
}
func TestModelTablesDeniedToRuntimeRoles(t *testing.T) {
	db, _, _ := modelFixture(t, 1)
	ctx := context.Background()
	passwords := map[string]string{}
	for component := range componentTables {
		passwords[component] = component + strings.Repeat("fixture-only", 4)
	}
	if err := db.ProvisionRoles(ctx, passwords); err != nil {
		t.Fatal(err)
	}
	for component := range componentTables {
		t.Run(component, func(t *testing.T) {
			tx, err := db.Pool.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			// Role names are fixed repository constants, never user input.
			role, roleErr := ComponentRole(component)
			if roleErr != nil {
				t.Fatal(roleErr)
			}
			if _, err = tx.ExecContext(ctx, "SET LOCAL ROLE "+role); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"model_budgets", "model_allocations", "model_calls"} {
				var allowed bool
				if err = tx.QueryRowContext(ctx, `SELECT has_table_privilege(current_user,$1,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')`, table).Scan(&allowed); err != nil {
					t.Fatal(err)
				}
				if allowed {
					t.Fatalf("%s can access %s", component, table)
				}
			}
			_, err = tx.QueryContext(ctx, `SELECT * FROM model_budgets`)
			if err == nil {
				t.Fatal("actual forbidden query succeeded")
			}
		})
	}
}

func TestModelExpiredOtherAttemptBlocksWithoutRecoverySweep(t *testing.T) {
	for _, phase := range []string{"reserved", "started"} {
		t.Run(phase, func(t *testing.T) {
			db, b, a := modelFixture(t, 3)
			ctx := context.Background()
			other := a
			other.ID = NewID()
			other.PodUID = NewID()
			other.Attempt = 2
			if err := db.CreateModelAllocation(ctx, other); err != nil {
				t.Fatal(err)
			}
			refA := modelReserve(t, db, a, 1)
			refB := modelReserve(t, db, other, 1)
			if phase == "started" {
				d, e := db.ClaimModelCall(ctx, refA)
				if e != nil || !d.Invoke {
					t.Fatalf("claim A %+v %v", d, e)
				}
			}
			if _, err := db.Pool.Exec(`UPDATE model_calls SET expires_at=clock_timestamp()-interval '1 second' WHERE allocation_id=$1`, a.ID); err != nil {
				t.Fatal(err)
			}
			// No recovery sweep is invoked before the second allocation attempts dispatch.
			d, e := db.ClaimModelCall(ctx, refB)
			if e != nil || d.Invoke || d.Reason != "inactive" {
				t.Fatalf("claim B bypassed expired A: %+v %v", d, e)
			}
			d, e = db.ReserveModelCall(ctx, other.ID, other.PodUID, 1, modelTestPayload)
			if e != nil || d.Reason != "inactive" {
				t.Fatalf("reserve B %+v %v", d, e)
			}
			var state string
			if err := db.Pool.QueryRow(`SELECT state FROM model_budgets WHERE work_item_uuid=$1`, b.WorkItem).Scan(&state); err != nil || state != "stopped" {
				t.Fatalf("parent %s %v", state, err)
			}
		})
	}
}

func TestModelExpiredSiblingClaimRacesRecovery(t *testing.T) {
	for range 10 {
		db, _, a := modelFixture(t, 3)
		ctx := context.Background()
		other := a
		other.ID = NewID()
		other.PodUID = NewID()
		other.Attempt = 2
		if err := db.CreateModelAllocation(ctx, other); err != nil {
			t.Fatal(err)
		}
		refA := modelReserve(t, db, a, 1)
		refB := modelReserve(t, db, other, 1)
		if d, e := db.ClaimModelCall(ctx, refA); e != nil || !d.Invoke {
			t.Fatalf("claim A %+v %v", d, e)
		}
		if _, err := db.Pool.Exec(`UPDATE model_calls SET expires_at=clock_timestamp()-interval '1 second' WHERE allocation_id=$1`, a.ID); err != nil {
			t.Fatal(err)
		}
		results := make(chan ModelDecision, 1)
		errs := make(chan error, 2)
		go func() { d, e := db.ClaimModelCall(ctx, refB); results <- d; errs <- e }()
		go func() { _, e := db.RecoverExpiredModelCalls(ctx); errs <- e }()
		d := <-results
		if d.Invoke || d.Reason != "inactive" {
			t.Fatalf("race claim %+v", d)
		}
		for range 2 {
			if e := <-errs; e != nil {
				t.Fatal(e)
			}
		}
	}
}
