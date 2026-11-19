package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func reservedRepairFixture(t *testing.T, lifetime time.Duration) (*DB, *DB, Lifecycle, ModelBudget, ModelAllocation, string) {
	t.Helper()
	db, parent, _, child := repairLedgerFixture(t)
	worker := repairRoles(t, db)["worker"]
	var l Lifecycle
	var config string
	if err := db.Pool.QueryRow(`SELECT r.child_request_id,m.config_digest FROM execution_repairs r JOIN execution_models m ON m.request_id=r.parent_candidate_id WHERE r.work_item_uuid=$1`, parent.WorkItem).Scan(&l.Claim.ID, &config); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(`DELETE FROM execution_models WHERE request_id=$1`, l.Claim.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(`DELETE FROM model_allocations WHERE id=$1`, child.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(`UPDATE repair_model_budgets SET state='reserved' WHERE work_item_uuid=$1`, parent.WorkItem); err != nil {
		t.Fatal(err)
	}
	l.Claim.Token, l.Claim.Until = NewID(), time.Now().Add(lifetime).Truncate(time.Microsecond)
	if _, err := db.Pool.Exec(`UPDATE execution_requests SET state='claimed',claim_token=$2,claim_until=$3 WHERE id=$1`, l.Claim.ID, l.Claim.Token, l.Claim.Until); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(`UPDATE execution_lifecycles SET claim_token=$2,claim_until=$3 WHERE request_id=$1`, l.Claim.ID, l.Claim.Token, l.Claim.Until); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(`SELECT binding_digest,source_digest,namespace,pod_name,config_name,source_bytes FROM execution_lifecycles WHERE request_id=$1`, l.Claim.ID).Scan(&l.Binding.BindingDigest, &l.Binding.SourceDigest, &l.Binding.Namespace, &l.Binding.PodName, &l.Binding.ConfigName, &l.Binding.SourceBytes); err != nil {
		t.Fatal(err)
	}
	b, allocation, err := worker.RepairModelReservation(context.Background(), l.Claim.ID, config)
	if err != nil {
		t.Fatal(err)
	}
	allocation.PodUID, allocation.ExpiresAt, b.ExpiresAt = child.PodUID, l.Claim.Until, l.Claim.Until
	return db, worker, l, b, allocation, config
}

func TestRepairActivationClampsClaimAndNeverAdopts(t *testing.T) {
	db, worker, l, b, a, config := reservedRepairFixture(t, time.Minute)
	var original string
	if err := db.Pool.QueryRow(`SELECT row_to_json(b)::text FROM model_budgets b WHERE work_item_uuid=$1`, b.WorkItem).Scan(&original); err != nil {
		t.Fatal(err)
	}
	scope, err := worker.ActivateRepairLifecycleModel(context.Background(), l, b, a, config)
	if err != nil || !scope.ExpiresAt.Equal(l.Claim.Until) || scope.AllocationID != a.ID || scope.Attempt != 2 {
		t.Fatal("derived activation changed original reservation or claim", err)
	}
	read, phase, err := worker.ReadLifecycleModel(context.Background(), l, config)
	if err != nil || phase != "source-ready" || read.AllocationID != a.ID || !read.ExpiresAt.Equal(scope.ExpiresAt) {
		t.Fatal("original repair owner cannot observe activation", err)
	}
	if _, err = worker.ActivateRepairLifecycleModel(context.Background(), l, b, a, config); err == nil {
		t.Fatal("already activated repair was adopted")
	}
	var current string
	if err := db.Pool.QueryRow(`SELECT row_to_json(b)::text FROM model_budgets b WHERE work_item_uuid=$1`, b.WorkItem).Scan(&current); err != nil || current != original {
		t.Fatal("activation modified original budget", err)
	}
	var expiry time.Time
	if err := db.Pool.QueryRow(`SELECT expires_at FROM repair_model_budgets WHERE work_item_uuid=$1`, b.WorkItem).Scan(&expiry); err != nil || !expiry.After(scope.ExpiresAt) {
		t.Fatal("short execution claim rewrote the original reservation deadline", err)
	}
}

func TestRepairActivationWaitRechecksExpiredClaim(t *testing.T) {
	db, worker, l, b, a, config := reservedRepairFixture(t, time.Second)
	ctx := context.Background()
	locked, err := db.Pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Rollback()
	if _, err = locked.Exec(`SELECT 1 FROM model_budgets WHERE work_item_uuid=$1 FOR UPDATE`, b.WorkItem); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, e := worker.ActivateRepairLifecycleModel(ctx, l, b, a, config); done <- e }()
	seen := false
	for end := time.Now().Add(500 * time.Millisecond); time.Now().Before(end); {
		if err = db.Pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE usename='aisdlc_worker' AND wait_event_type='Lock' AND query LIKE 'SELECT work_item_uuid FROM model_budgets%')`).Scan(&seen); err != nil {
			t.Fatal(err)
		}
		if seen {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !seen {
		t.Fatal("real activation parent-lock wait not observed")
	}
	if remaining := time.Until(l.Claim.Until) + 20*time.Millisecond; remaining > 0 {
		time.Sleep(remaining)
	}
	if err = locked.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err == nil {
		t.Fatal("expired waiting claim activated repair")
	}
	var state string
	var count int
	if err = db.Pool.QueryRow(`SELECT state,(SELECT count(*) FROM model_allocations WHERE id=$2) FROM repair_model_budgets WHERE work_item_uuid=$1`, b.WorkItem, a.ID).Scan(&state, &count); err != nil || state != "reserved" || count != 0 {
		t.Fatal("failed activation committed allowance or allocation", err)
	}
}

func TestRepairRecoveryStopsOnlyItsOwnAttempt(t *testing.T) {
	db, worker, l, b, a, config := reservedRepairFixture(t, time.Minute)
	if _, err := worker.ActivateRepairLifecycleModel(context.Background(), l, b, a, config); err != nil {
		t.Fatal(err)
	}
	ref := modelReserve(t, worker, a, 1)
	if err := worker.ExpireExecutionClaims(context.Background()); err != nil {
		t.Fatal(err)
	}
	var state, original string
	if err := db.Pool.QueryRow(`SELECT state FROM model_calls WHERE allocation_id=$1 AND sequence=1`, ref.AllocationID).Scan(&state); err != nil || state != "reserved" {
		t.Fatal("old parent recovery touched child call", err)
	}
	if _, err := db.Pool.Exec(`UPDATE execution_requests SET claim_until=clock_timestamp()-interval '1 second' WHERE id=$1`, l.Claim.ID); err != nil {
		t.Fatal(err)
	}
	if err := worker.ExpireExecutionClaims(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(`SELECT r.state,b.stop_reason FROM repair_model_budgets r JOIN model_budgets b USING(work_item_uuid) WHERE work_item_uuid=$1`, b.WorkItem).Scan(&state, &original); err != nil || state != "stopped" || original != "candidate-captured" {
		t.Fatal("child recovery changed root or failed to stop", err)
	}
	if err := db.Pool.QueryRow(`SELECT state FROM model_calls WHERE allocation_id=$1 AND sequence=1`, ref.AllocationID).Scan(&state); err != nil || state != "stopped" {
		t.Fatal("expired child call remained active", err)
	}
}

func TestRepairCaptureSealsOnlyChildAndRetainsHistoricalReader(t *testing.T) {
	db, worker, l, b, a, config := reservedRepairFixture(t, time.Minute)
	if _, err := worker.ActivateRepairLifecycleModel(context.Background(), l, b, a, config); err != nil {
		t.Fatal(err)
	}
	var originalBudget, originalCandidate string
	if err := db.Pool.QueryRow(`SELECT row_to_json(b)::text,row_to_json(c)::text FROM model_budgets b JOIN execution_repairs r USING(work_item_uuid) JOIN execution_candidates c ON c.request_id=r.parent_candidate_id WHERE b.work_item_uuid=$1`, b.WorkItem).Scan(&originalBudget, &originalCandidate); err != nil {
		t.Fatal(err)
	}
	// A separate completed parent call ensures child capture cannot count all
	// calls of the shared work item. This remains explicitly synthetic history.
	if _, err := db.Pool.Exec(`INSERT INTO model_calls(allocation_id,sequence,request_sha256,input_bytes,response_limit,expires_at,state,output_bytes,response_sha256,response_status,response_handed_off_at)
 SELECT parent_allocation_id,1,repeat('a',64),100,1024,expires_at,'completed',2,repeat('b',64),200,clock_timestamp() FROM execution_repairs WHERE work_item_uuid=$1`, b.WorkItem); err != nil {
		t.Fatal(err)
	}
	for sequence := 1; sequence <= 2; sequence++ {
		ref := modelReserve(t, worker, a, sequence)
		if d, err := worker.ClaimModelCall(context.Background(), ref); err != nil || !d.Invoke {
			t.Fatal("child call claim", err)
		}
		response := []byte(`{"choices":[{"message":{"role":"assistant","content":"fixture"}}]}`)
		if _, err := worker.FinishModelCall(context.Background(), ref, 200, response); err != nil {
			t.Fatal(err)
		}
		if _, err := worker.AcknowledgeModelResponse(context.Background(), ref, authority.Digest(response), 200, len(response)); err != nil {
			t.Fatal(err)
		}
	}
	source := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: b.SourceSHA, Files: []workspacebundle.File{{Path: "example.txt", Mode: 0644, Data: []byte("original")}}}
	result := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: b.SourceSHA, Files: []workspacebundle.File{{Path: "example.txt", Mode: 0644, Data: []byte("repaired")}}}
	delta, err := candidate.Build(source, result)
	if err != nil {
		t.Fatal(err)
	}
	deltaRaw, err := candidate.Encode(delta)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := workspacebundle.Encode(result)
	if err != nil {
		t.Fatal(err)
	}
	zero := int32(0)
	receipt, _ := json.Marshal(LifecycleResult{UID: a.PodUID, Digest: workspacebundle.Digest(exported), Bytes: len(exported), ExitCode: &zero})
	configUID := NewID()
	if _, err = db.Pool.Exec(`UPDATE execution_lifecycles SET phase='export-ready',config_uid=$2 WHERE request_id=$1`, l.Claim.ID, configUID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Pool.Exec(`INSERT INTO execution_effects(request_id,kind,token,result,completed_at) VALUES($1,'export',$2,$3,clock_timestamp())`, l.Claim.ID, NewID(), receipt); err != nil {
		t.Fatal(err)
	}
	record, err := worker.CaptureLifecycleCandidate(context.Background(), l, a.ID, exported, deltaRaw)
	if err != nil || record.ModelCalls != 2 || record.SourceTreeSHA256 != delta.SourceTreeSHA256 {
		t.Fatal("child capture reused parent call history", record.ModelCalls, err)
	}
	if err = worker.ConfirmExecutionCleanup(context.Background(), l, configUID, a.PodUID); err != nil {
		t.Fatal(err)
	}
	read, err := worker.ReadCapturedCandidate(context.Background(), l.Claim.ID)
	if err != nil || read.Record != record {
		t.Fatal("closed child historical reader failed", err)
	}
	var budgetAfter, candidateAfter, state, reason string
	if err = db.Pool.QueryRow(`SELECT row_to_json(b)::text,row_to_json(c)::text,rb.state,rb.stop_reason FROM model_budgets b JOIN execution_repairs r USING(work_item_uuid) JOIN execution_candidates c ON c.request_id=r.parent_candidate_id JOIN repair_model_budgets rb USING(work_item_uuid) WHERE b.work_item_uuid=$1`, b.WorkItem).Scan(&budgetAfter, &candidateAfter, &state, &reason); err != nil || budgetAfter != originalBudget || candidateAfter != originalCandidate || state != "stopped" || reason != "candidate-captured" {
		t.Fatal("child capture changed parent or failed to seal child", err)
	}
	if _, err = db.Pool.Exec(`UPDATE repair_model_budgets SET stop_reason='different' WHERE work_item_uuid=$1`, b.WorkItem); err != nil {
		t.Fatal(err)
	}
	if _, err = worker.ReadCapturedCandidate(context.Background(), l.Claim.ID); err == nil {
		t.Fatal("child reader fell back to sealed parent")
	}
}
