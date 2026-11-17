package store

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func requireSQLState(t *testing.T, err error, want string) {
	t.Helper()
	var coded interface{ SQLState() string }
	if !errors.As(err, &coded) || coded.SQLState() != want {
		t.Fatalf("expected PostgreSQL SQLSTATE %s, got %v", want, err)
	}
}

func repairRoles(t *testing.T, db *DB) map[string]*DB {
	t.Helper()
	passwords := map[string]string{}
	for component := range componentTables {
		passwords[component] = strings.ReplaceAll(component, "-", "_") + strings.Repeat("x", 40)
	}
	if err := db.ProvisionRolesForOptions(context.Background(), passwords, "verification", true); err != nil {
		t.Fatal(err)
	}
	roles := map[string]*DB{}
	for _, component := range []string{"orchestrator", "worker", "verifier", "relay", "release-manager"} {
		u, err := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		role, _ := ComponentRole(component)
		u.User = url.UserPassword(role, passwords[component])
		connected, err := Open(context.Background(), u.String())
		if err != nil {
			t.Fatal("component connection failed")
		}
		if err = AssertComponent(context.Background(), connected.Pool, component); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = connected.Close() })
		roles[component] = connected
	}
	return roles
}

func TestRepairParentLockHasNoMutationAuthority(t *testing.T) {
	db, b, _ := modelFixture(t, 4)
	roles := repairRoles(t, db)
	ctx := context.Background()
	orch := roles["orchestrator"]
	_, err := orch.Pool.ExecContext(ctx, `SELECT public.lock_repair_parent_budget($1)`, b.WorkItem)
	requireSQLState(t, err, "55000")
	if _, err := db.Pool.ExecContext(ctx, `UPDATE model_budgets SET state='stopped',stop_reason='candidate-captured' WHERE work_item_uuid=$1`, b.WorkItem); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Pool.ExecContext(ctx, `SELECT public.lock_repair_parent_budget($1)`, b.WorkItem); err != nil {
		t.Fatal("closed parent lock denied", err)
	}
	for component, role := range roles {
		if component != "orchestrator" {
			_, err := role.Pool.ExecContext(ctx, `SELECT public.lock_repair_parent_budget($1)`, b.WorkItem)
			requireSQLState(t, err, "42501")
		}
		_, err := role.Pool.ExecContext(ctx, `CREATE OR REPLACE FUNCTION public.lock_repair_parent_budget(uuid) RETURNS void LANGUAGE sql AS 'SELECT NULL::void'`)
		requireSQLState(t, err, "42501")
	}
	for _, statement := range []string{`UPDATE model_budgets SET max_calls=max_calls WHERE work_item_uuid=$1`,
		`UPDATE model_budgets SET reserved_calls=reserved_calls WHERE work_item_uuid=$1`,
		`UPDATE model_budgets SET expires_at=expires_at WHERE work_item_uuid=$1`,
		`UPDATE model_budgets SET state=state WHERE work_item_uuid=$1`,
		`UPDATE model_allocations SET reserved_calls=reserved_calls WHERE work_item_uuid=$1`} {
		_, err := orch.Pool.ExecContext(ctx, statement, b.WorkItem)
		requireSQLState(t, err, "42501")
	}
	for _, statement := range []string{
		`INSERT INTO execution_repairs(created_at) VALUES(clock_timestamp())`,
		`INSERT INTO repair_model_budgets(reserved_calls) VALUES(0)`,
		`INSERT INTO repair_model_budgets(reserved_input_bytes) VALUES(0)`,
		`INSERT INTO repair_model_budgets(reserved_output_bytes) VALUES(0)`,
		`INSERT INTO repair_model_budgets(state) VALUES('active')`,
		`INSERT INTO repair_model_budgets(stop_reason) VALUES('')`,
	} {
		_, err := orch.Pool.ExecContext(ctx, statement)
		requireSQLState(t, err, "42501")
	}
	var valid bool
	if err := db.Pool.QueryRowContext(ctx, `SELECT p.prosecdef AND p.provolatile='v'
 AND p.proconfig=ARRAY['search_path=pg_catalog'] AND p.proowner=c.relowner
 AND pg_get_userbyid(p.proowner) NOT LIKE 'aisdlc_%'
 AND NOT EXISTS(SELECT 1 FROM aclexplode(p.proacl) a WHERE a.grantee=0 AND a.privilege_type='EXECUTE')
 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 JOIN pg_class c ON c.oid='public.model_budgets'::regclass
 WHERE n.nspname='public' AND p.proname='lock_repair_parent_budget'`).Scan(&valid); err != nil || !valid {
		t.Fatal("helper ownership or public privilege differs", err)
	}
	// A temporary object cannot redirect the privileged table lookup.
	tx, err := orch.Pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE TEMP TABLE model_budgets(work_item_uuid uuid,state text,stop_reason text)`); err != nil {
		t.Fatal(err)
	}
	foreign := NewID()
	if _, err = tx.ExecContext(ctx, `INSERT INTO pg_temp.model_budgets VALUES($1,'stopped','candidate-captured')`, foreign); err != nil {
		t.Fatal(err)
	}
	_, err = tx.ExecContext(ctx, `SELECT public.lock_repair_parent_budget($1)`, foreign)
	requireSQLState(t, err, "55000")
}

func TestRepairParentLockWaitCancelsAndReleases(t *testing.T) {
	db, b, _ := modelFixture(t, 4)
	roles := repairRoles(t, db)
	ctx := context.Background()
	if _, err := db.Pool.ExecContext(ctx, `UPDATE model_budgets SET state='stopped',stop_reason='candidate-captured' WHERE work_item_uuid=$1`, b.WorkItem); err != nil {
		t.Fatal(err)
	}
	orch := roles["orchestrator"]
	holder, err := orch.Pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	if _, err = holder.ExecContext(ctx, `SELECT public.lock_repair_parent_budget($1)`, b.WorkItem); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := orch.Pool.ExecContext(waitCtx, `SELECT public.lock_repair_parent_budget($1)`, b.WorkItem)
		done <- err
	}()
	seen := false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := db.Pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE usename='aisdlc_orchestrator' AND wait_event_type='Lock' AND query LIKE '%lock_repair_parent_budget%')`).Scan(&seen); err != nil {
			t.Fatal(err)
		}
		if seen {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !seen {
		t.Fatal("real database lock wait not observed")
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled lock waiter succeeded")
	}
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	bounded, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if _, err := orch.Pool.ExecContext(bounded, `SELECT public.lock_repair_parent_budget($1)`, b.WorkItem); err != nil {
		t.Fatal("rollback did not release parent lock", err)
	}
	var count int
	if err := db.Pool.QueryRowContext(ctx, `SELECT count(*) FROM execution_repairs WHERE work_item_uuid=$1`, b.WorkItem).Scan(&count); err != nil || count != 0 {
		t.Fatal("lock helper created a derivation")
	}
	if err := db.ProvisionRolesForMode(ctx, mapRolePasswords(), "maintenance"); err != nil {
		t.Fatal(err)
	}
	_, err = orch.Pool.ExecContext(ctx, `SELECT public.lock_repair_parent_budget($1)`, b.WorkItem)
	requireSQLState(t, err, "42501")
}

func mapRolePasswords() map[string]string {
	result := map[string]string{}
	for component := range componentTables {
		result[component] = strings.ReplaceAll(component, "-", "_") + strings.Repeat("x", 40)
	}
	return result
}
