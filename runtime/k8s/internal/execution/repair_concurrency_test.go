package execution_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repairflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func waitRepairSQLLock(t *testing.T, f execution.RepairFixtureForTest, blocker int) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var pid int
		e := f.Admin.Pool.QueryRow(`SELECT pid FROM pg_stat_activity WHERE usename='aisdlc_orchestrator' AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)) LIMIT 1`, blocker).Scan(&pid)
		if e == nil {
			return pid
		}
		if e != sql.ErrNoRows {
			t.Fatal(e)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("actual rejection/planner SQL wait not observed")
	return 0
}
func pauseRepairClosure(t *testing.T, f execution.RepairFixtureForTest) (*sql.Tx, int) {
	t.Helper()
	if _, e := f.Admin.Pool.Exec(`CREATE FUNCTION pause_rejection_close() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status='failed' THEN PERFORM pg_advisory_xact_lock(637001); END IF;RETURN NEW;END $$;
 CREATE TRIGGER pause_rejection_close AFTER UPDATE ON work_items FOR EACH ROW EXECUTE FUNCTION pause_rejection_close()`); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_, _ = f.Admin.Pool.Exec(`DROP TRIGGER IF EXISTS pause_rejection_close ON work_items;DROP FUNCTION IF EXISTS pause_rejection_close()`)
	})
	tx, e := f.Admin.Pool.BeginTx(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(`SELECT pg_advisory_xact_lock(637001)`); e != nil {
		tx.Rollback()
		t.Fatal(e)
	}
	var pid int
	if e = tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); e != nil {
		tx.Rollback()
		t.Fatal(e)
	}
	return tx, pid
}
func terminalRepairFixture(t *testing.T) execution.RepairFixtureForTest {
	t.Helper()
	f := execution.NewRepairFixtureForTest(t, false)
	if _, e := repairflow.Handoff(context.Background(), f.Verifier, repairLoad(f, "verifier")); e != nil {
		t.Fatal(e)
	}
	return f
}
func TestConnectedRepairTerminalSerializesPlannerWithdrawal(t *testing.T) {
	for _, order := range []string{"planner-first", "stop-first"} {
		t.Run(order, func(t *testing.T) {
			f := terminalRepairFixture(t)
			ctx := context.Background()
			done := make(chan error, 1)
			planner := make(chan error, 1)
			if order == "planner-first" {
				tx, e := f.Orchestrator.Pool.BeginTx(ctx, nil)
				if e != nil {
					t.Fatal(e)
				}
				defer tx.Rollback()
				var pid int
				if e = tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); e != nil {
					t.Fatal(e)
				}
				if e = store.BlockSupersededIssue(ctx, tx, f.Command.Repository, f.Command.IssueNumber); e != nil {
					t.Fatal(e)
				}
				go func() { _, e := relayRepair(t, f, "stop-rejected-work", nil); done <- e }()
				waitRepairSQLLock(t, f, pid)
				if e = tx.Commit(); e != nil {
					t.Fatal(e)
				}
			} else {
				tx, pid := pauseRepairClosure(t, f)
				defer tx.Rollback()
				go func() { _, e := relayRepair(t, f, "stop-rejected-work", nil); done <- e }()
				stopPID := waitRepairSQLLock(t, f, pid)
				go func() {
					planner <- f.Orchestrator.WithTx(ctx, func(tx *sql.Tx) error {
						return store.BlockSupersededIssue(ctx, tx, f.Command.Repository, f.Command.IssueNumber)
					})
				}()
				waitRepairSQLLock(t, f, stopPID)
				if e := tx.Commit(); e != nil {
					t.Fatal(e)
				}
				if e := <-planner; e != nil {
					t.Fatal(e)
				}
			}
			if e := <-done; e != nil {
				t.Fatal(e)
			}
			expected := "failed"
			if order == "planner-first" {
				expected = "blocked"
			}
			var state string
			if e := f.Admin.Pool.QueryRow(`SELECT status FROM work_items WHERE uuid=$1`, f.Command.WorkItemID).Scan(&state); e != nil || state != expected {
				t.Fatal("concurrent withdrawal overwritten", state, e)
			}
		})
	}
}
func TestConnectedRepairChangedHeadDuringCloseRollsBackInbox(t *testing.T) {
	f := terminalRepairFixture(t)
	tx, pid := pauseRepairClosure(t, f)
	defer tx.Rollback()
	done := make(chan error, 1)
	go func() { _, e := relayRepair(t, f, "stop-rejected-work", nil); done <- e }()
	waitRepairSQLLock(t, f, pid)
	if _, e := f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
		t.Fatal(e)
	}
	if e := tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if e := <-done; e == nil {
		t.Fatal("changed head accepted after delayed UPDATE")
	}
	var state string
	var accepted int
	if e := f.Admin.Pool.QueryRow(`SELECT status,(SELECT count(*) FROM authorized_inbox WHERE event_id LIKE 'rejection-%') FROM work_items WHERE uuid=$1`, f.Command.WorkItemID).Scan(&state, &accepted); e != nil || state != "running" || accepted != 0 {
		t.Fatal("partial rejection commit", state, accepted, e)
	}
	if duplicate, e := relayRepair(t, f, "stop-rejected-work", nil); e != nil || duplicate {
		t.Fatal("superseded retry", duplicate, e)
	}
	if e := f.Admin.Pool.QueryRow(`SELECT status FROM work_items WHERE uuid=$1`, f.Command.WorkItemID).Scan(&state); e != nil || state != "running" {
		t.Fatal("superseded work mutated", state, e)
	}
}
