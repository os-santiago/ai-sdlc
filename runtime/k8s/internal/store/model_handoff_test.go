package store

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
)

var handoffBody = []byte(`{"choices":[{"message":{"content":"fixture"}}]}`)

func modelComplete(t *testing.T, db *DB, ref ModelCallRef) {
	t.Helper()
	ctx := context.Background()
	if d, err := db.ClaimModelCall(ctx, ref); err != nil || !d.Invoke {
		t.Fatalf("claim: %+v %v", d, err)
	}
	if d, err := db.FinishModelCall(ctx, ref, 200, handoffBody); err != nil || d.Reason != "completed" || d.Call.ResponseHandedOff {
		t.Fatalf("finish: %+v %v", d, err)
	}
}
func modelAck(t *testing.T, db *DB, ref ModelCallRef) ModelDecision {
	t.Helper()
	d, err := db.AcknowledgeModelResponse(context.Background(), ref, modelmailbox.Digest(handoffBody), 200, len(handoffBody))
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func TestModelHandoffBlocksNewReservationsAndExistingClaimsAcrossBudget(t *testing.T) {
	db, _, a := modelFixture(t, 4)
	ctx := context.Background()
	b := a
	b.ID = NewID()
	b.PodUID = NewID()
	b.Attempt = 2
	if err := db.CreateModelAllocation(ctx, b); err != nil {
		t.Fatal(err)
	}
	first := modelReserve(t, db, a, 1)
	alreadyReserved := modelReserve(t, db, b, 1)
	modelComplete(t, db, first)
	for i := 0; i < 10; i++ {
		if d, err := db.ReserveModelCall(ctx, a.ID, a.PodUID, 2, modelTestPayload); err != nil || d.Reason != "response-handoff-pending" {
			t.Fatalf("new call passed barrier: %+v %v", d, err)
		}
		if d, err := db.ClaimModelCall(ctx, alreadyReserved); err != nil || d.Invoke || d.Reason != "response-handoff-pending" {
			t.Fatalf("prior reservation passed barrier: %+v %v", d, err)
		}
	}
	if d := modelAck(t, db, first); d.Reason != "handed-off" || d.Invoke || !d.Call.ResponseHandedOff {
		t.Fatal(d)
	}
	if d, err := db.ClaimModelCall(ctx, alreadyReserved); err != nil || !d.Invoke {
		t.Fatalf("ack did not release valid prior claim: %+v %v", d, err)
	}
	if d, err := db.ReserveModelCall(ctx, a.ID, a.PodUID, 2, modelTestPayload); err != nil || d.Reason != "reserved" {
		t.Fatalf("ack did not release valid next reservation: %+v %v", d, err)
	}
}
func TestModelHandoffDuplicateIsImmutableMetadataOnly(t *testing.T) {
	db, b, a := modelFixture(t, 2)
	ref := modelReserve(t, db, a, 1)
	modelComplete(t, db, ref)
	if d := modelAck(t, db, ref); d.Reason != "handed-off" {
		t.Fatal(d)
	}
	var before, after time.Time
	if err := db.Pool.QueryRow(`SELECT response_handed_off_at FROM model_calls WHERE allocation_id=$1`, a.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := db.AcknowledgeModelResponse(context.Background(), ref, modelmailbox.Digest(handoffBody), 200, len(handoffBody))
			if err != nil || d.Reason != "existing" || d.Invoke {
				t.Errorf("repeat: %+v %v", d, err)
			}
		}()
	}
	wg.Wait()
	if err := db.Pool.QueryRow(`SELECT response_handed_off_at FROM model_calls WHERE allocation_id=$1`, a.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !before.Equal(after) {
		t.Fatal("handoff timestamp changed")
	}
	if _, err := db.Pool.Exec(`UPDATE model_budgets SET state='stopped' WHERE work_item_uuid=$1`, b.WorkItem); err != nil {
		t.Fatal(err)
	}
	if d := modelAck(t, db, ref); d.Reason != "existing" || d.Invoke {
		t.Fatal(d)
	}
	var state string
	if err := db.Pool.QueryRow(`SELECT state FROM model_budgets WHERE work_item_uuid=$1`, b.WorkItem).Scan(&state); err != nil || state != "stopped" {
		t.Fatal("ack reopened budget")
	}
}
func TestModelHandoffMismatchStopsParent(t *testing.T) {
	for _, kind := range []string{"response-hash", "response-size", "response-status", "request-hash", "pod"} {
		t.Run(kind, func(t *testing.T) {
			db, b, a := modelFixture(t, 2)
			ref := modelReserve(t, db, a, 1)
			modelComplete(t, db, ref)
			hash, status, size := modelmailbox.Digest(handoffBody), 200, len(handoffBody)
			switch kind {
			case "response-hash":
				hash = strings.Repeat("f", 64)
			case "response-size":
				size++
			case "response-status":
				status = 500
			case "request-hash":
				ref.RequestSHA256 = strings.Repeat("e", 64)
			case "pod":
				ref.PodUID = NewID()
			}
			d, err := db.AcknowledgeModelResponse(context.Background(), ref, hash, status, size)
			if err != nil || d.Invoke || d.Call.ResponseHandedOff {
				t.Fatalf("mismatch: %+v %v", d, err)
			}
			var state string
			if err := db.Pool.QueryRow(`SELECT state FROM model_budgets WHERE work_item_uuid=$1`, b.WorkItem).Scan(&state); err != nil || state != "stopped" {
				t.Fatalf("budget not stopped: %s %v", state, err)
			}
		})
	}
}
func TestModelHandoffAbandonmentPreservesResponseEvidence(t *testing.T) {
	for _, expire := range []bool{false, true} {
		db, b, a := modelFixture(t, 2)
		ref := modelReserve(t, db, a, 1)
		modelComplete(t, db, ref)
		if expire {
			if _, err := db.Pool.Exec(`UPDATE model_calls SET expires_at=clock_timestamp()-interval '1 second' WHERE allocation_id=$1`, a.ID); err != nil {
				t.Fatal(err)
			}
			if n, err := db.RecoverExpiredModelCalls(context.Background()); err != nil || n < 1 {
				t.Fatalf("recover %d %v", n, err)
			}
		} else if d, err := db.StopModelCall(context.Background(), ref); err != nil || d.Reason != "ambiguous" {
			t.Fatalf("stop %+v %v", d, err)
		}
		var state, hash string
		var size, status int
		var handed bool
		if err := db.Pool.QueryRow(`SELECT state,response_sha256,output_bytes,response_status,response_handed_off_at IS NOT NULL FROM model_calls WHERE allocation_id=$1`, a.ID).Scan(&state, &hash, &size, &status, &handed); err != nil {
			t.Fatal(err)
		}
		if state != "ambiguous" || hash != modelmailbox.Digest(handoffBody) || size != len(handoffBody) || status != 200 || handed {
			t.Fatal("response evidence discarded or handoff invented")
		}
		if d := modelAck(t, db, ref); d.Reason != "not-completed" || d.Invoke {
			t.Fatal(d)
		}
		if d, err := db.ReserveModelCall(context.Background(), a.ID, a.PodUID, 2, modelTestPayload); err != nil || d.Reason != "inactive" {
			t.Fatalf("abandoned budget still active: %+v %v", d, err)
		}
		calls, _, output := modelCount(t, db, b.WorkItem)
		if calls != 1 || output != 1024 {
			t.Fatal("abandoned call was refunded")
		}
	}
}
func TestModelHandoffAndAbandonmentRace(t *testing.T) {
	for range 10 {
		db, b, a := modelFixture(t, 2)
		ref := modelReserve(t, db, a, 1)
		modelComplete(t, db, ref)
		errs := make(chan error, 2)
		go func() { _, err := db.StopModelCall(context.Background(), ref); errs <- err }()
		go func() {
			_, err := db.AcknowledgeModelResponse(context.Background(), ref, modelmailbox.Digest(handoffBody), 200, len(handoffBody))
			errs <- err
		}()
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		var state, parent string
		var handed bool
		if err := db.Pool.QueryRow(`SELECT c.state,c.response_handed_off_at IS NOT NULL,b.state FROM model_calls c JOIN model_allocations a ON a.id=c.allocation_id JOIN model_budgets b ON b.work_item_uuid=a.work_item_uuid WHERE c.allocation_id=$1`, a.ID).Scan(&state, &handed, &parent); err != nil {
			t.Fatal(err)
		}
		if !((state == "completed" && handed && parent == "active") || (state == "ambiguous" && !handed && parent == "stopped")) {
			t.Fatalf("inconsistent outcome %s %t %s", state, handed, parent)
		}
		calls, _, _ := modelCount(t, db, b.WorkItem)
		if calls != 1 {
			t.Fatal("race reset counters")
		}
	}
}

func TestModelHandoffExpiresAfterFinalRowLocks(t *testing.T) {
	for _, lockedTable := range []string{"budget", "allocation", "call"} {
		t.Run(lockedTable, func(t *testing.T) {
			db, b, a := modelFixture(t, 3)
			ref := modelReserve(t, db, a, 1)
			modelComplete(t, db, ref)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var expires time.Time
			if err := db.Pool.QueryRowContext(ctx, `UPDATE model_calls SET expires_at=clock_timestamp()+interval '1 second' WHERE allocation_id=$1 RETURNING expires_at`, a.ID).Scan(&expires); err != nil {
				t.Fatal(err)
			}
			blocker, err := db.Pool.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			lockSQL, key, prefix := `SELECT 1 FROM model_budgets WHERE work_item_uuid=$1 FOR UPDATE`, b.WorkItem, "SELECT model,state,max_calls,%"
			if lockedTable == "allocation" {
				lockSQL, key, prefix = `SELECT 1 FROM model_allocations WHERE id=$1 FOR UPDATE`, a.ID, "SELECT pod_uid,max_calls,%"
			}
			if lockedTable == "call" {
				lockSQL, key, prefix = `SELECT 1 FROM model_calls WHERE allocation_id=$1 FOR UPDATE`, a.ID, "SELECT request_sha256,state,input_bytes,%"
			}
			if _, err = blocker.ExecContext(ctx, lockSQL, key); err != nil {
				t.Fatal(err)
			}
			result := make(chan ModelDecision, 1)
			errs := make(chan error, 1)
			go func() {
				d, err := db.AcknowledgeModelResponse(ctx, ref, modelmailbox.Digest(handoffBody), 200, len(handoffBody))
				result <- d
				errs <- err
			}()
			for {
				var blocked bool
				if err = db.Pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE $1)`, prefix).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("ack did not block on expected row")
				case <-time.After(5 * time.Millisecond):
				}
			}
			if _, err = blocker.ExecContext(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp())))+0.05)`, expires); err != nil {
				t.Fatal(err)
			}
			if err = blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			d := <-result
			if err = <-errs; err != nil || d.Invoke || d.Call.ResponseHandedOff || d.Reason != "uncertain-handoff" || d.Call.State != "ambiguous" {
				t.Fatalf("expired handoff accepted: %+v %v", d, err)
			}
		})
	}
}
func TestModelExpiredUnhandedResponseStopsSiblingBeforeSweep(t *testing.T) {
	db, _, a := modelFixture(t, 3)
	ctx := context.Background()
	b := a
	b.ID = NewID()
	b.PodUID = NewID()
	b.Attempt = 2
	if err := db.CreateModelAllocation(ctx, b); err != nil {
		t.Fatal(err)
	}
	first := modelReserve(t, db, a, 1)
	second := modelReserve(t, db, b, 1)
	modelComplete(t, db, first)
	if _, err := db.Pool.Exec(`UPDATE model_calls SET expires_at=clock_timestamp()-interval '1 second' WHERE allocation_id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if d, err := db.ClaimModelCall(ctx, second); err != nil || d.Invoke || d.Reason != "inactive" {
		t.Fatalf("claim bypassed expired handoff: %+v %v", d, err)
	}
	if d, err := db.ReserveModelCall(ctx, b.ID, b.PodUID, 1, modelTestPayload); err != nil || d.Reason != "inactive" {
		t.Fatalf("reservation bypassed expired handoff: %+v %v", d, err)
	}
}
