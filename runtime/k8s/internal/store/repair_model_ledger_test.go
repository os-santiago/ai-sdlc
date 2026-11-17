package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

// Synthetic SQL establishes ledger history only. These bytes are deliberately
// not authenticated workflow evidence; runtime integration tests exercise that
// separate boundary.
func repairLedgerFixture(t *testing.T) (*DB, ModelBudget, ModelAllocation, ModelAllocation) {
	t.Helper()
	db, b, parent := modelFixture(t, 4)
	digest := authority.Digest([]byte(NewID()))
	childDigest := authority.Digest([]byte(NewID()))
	child := ModelAllocation{ID: NewID(), WorkItem: b.WorkItem, PodUID: NewID(), Attempt: 2,
		Limits: ModelLimits{2, 4096, 2048}, ResponseLimit: parent.ResponseLimit,
		ExpiresAt: time.Now().Add(10 * time.Minute).Truncate(time.Millisecond)}
	seed := fmt.Sprintf(`
UPDATE model_budgets SET state='stopped',stop_reason='candidate-captured',reserved_calls=1,reserved_input_bytes=100,reserved_output_bytes=1024 WHERE work_item_uuid='%[1]s';
INSERT INTO execution_requests(id,work_item_uuid,attempt,repository,authority_revision,payload_digest,payload,receipt,expires_at,state,stop_reason)
 VALUES('execution-%[2]s','%[1]s',1,'fixture/product','%[2]s','%[2]s','{}','{}',clock_timestamp()+interval '15 minutes','stopped','uncertain'),
 ('execution-%[3]s','%[1]s',2,'fixture/product','%[2]s','%[3]s','{}','{}',clock_timestamp()+interval '15 minutes','queued','');
INSERT INTO execution_lifecycles(request_id,claim_token,claim_until,binding_digest,source_digest,source_bytes,namespace,pod_name,config_name,phase,pod_uid,cleanup_state)
 VALUES('execution-%[2]s','%[5]s',clock_timestamp()+interval '10 minutes','%[2]s','%[2]s',1,'ai-sdlc-executors','executor-%[1]s-1','assignment-'||substr('%[2]s',1,32),'candidate-captured','%[5]s','complete'),
 ('execution-%[3]s','%[8]s',clock_timestamp()+interval '10 minutes','%[3]s','%[3]s',1,'ai-sdlc-executors','executor-%[1]s-2','assignment-'||substr('%[3]s',1,32),'source-ready','%[8]s','pending');
INSERT INTO execution_models(request_id,allocation_id,config_digest) VALUES('execution-%[2]s','%[4]s','%[2]s');
INSERT INTO execution_candidates(request_id,allocation_id,pod_uid,source_sha,source_tree_sha256,candidate_tree_sha256,export_sha256,export_bytes,delta_sha256,delta,changes,model_calls)
 VALUES('execution-%[2]s','%[4]s','%[5]s','%[6]s','%[2]s','%[3]s','%[2]s',1,'%[2]s','{}',1,1);
INSERT INTO verification_requests(id,candidate_id,payload_digest,payload,receipt,expires_at,state,cleanup_state,proof,claim_token,claim_until)
 VALUES('verification-%[2]s','execution-%[2]s','%[2]s','{}','{}',clock_timestamp()+interval '15 minutes','passed','complete','{}','%[5]s',clock_timestamp()+interval '10 minutes');
INSERT INTO review_runs(id,verification_id,candidate_id,work_item_uuid,behavior_digest,profile_digest,configuration_digest,assignment_digest,expires_at,state,reason,cleanup_state)
 VALUES('review-%[2]s','verification-%[2]s','execution-%[2]s','%[1]s','%[2]s','%[2]s','%[2]s','%[2]s',clock_timestamp()+interval '15 minutes','failed','request-changes','complete');
INSERT INTO review_feedback(review_id,decision,digest) VALUES('review-%[2]s','{}','%[2]s');
INSERT INTO review_repair_handoffs(review_id,work_item_uuid,outcome,payload,payload_digest) VALUES('review-%[2]s','%[1]s','forwarded','{}','%[2]s');
INSERT INTO execution_repairs(work_item_uuid,parent_review_id,parent_candidate_id,parent_allocation_id,child_request_id,child_allocation_id,handoff_digest,child_payload,expires_at)
 VALUES('%[1]s','review-%[2]s','execution-%[2]s','%[4]s','execution-%[3]s','%[7]s','%[2]s','{}','%[9]s');
INSERT INTO repair_model_budgets(work_item_uuid,allocation_id,max_calls,max_input_bytes,max_output_bytes,response_limit,expires_at,state)
 VALUES('%[1]s','%[7]s',2,4096,2048,1024,'%[9]s','active');
INSERT INTO model_allocations(id,work_item_uuid,attempt,pod_uid,max_calls,max_input_bytes,max_output_bytes,response_limit,expires_at)
 VALUES('%[7]s','%[1]s',2,'%[8]s',2,4096,2048,1024,'%[9]s');
INSERT INTO execution_models(request_id,allocation_id,config_digest) VALUES('execution-%[3]s','%[7]s','%[3]s');
`, b.WorkItem, digest, childDigest, parent.ID, parent.PodUID, b.SourceSHA, child.ID, child.PodUID, child.ExpiresAt.Format(time.RFC3339Nano))
	if _, err := db.Pool.Exec(seed); err != nil {
		t.Fatal(err)
	}
	return db, b, parent, child
}

func TestRepairLedgerSpendsOnlyDerivedAllowance(t *testing.T) {
	db, b, parent, child := repairLedgerFixture(t)
	roles := repairRoles(t, db)
	worker := roles["worker"]
	ctx := context.Background()
	var before string
	if err := db.Pool.QueryRow(`SELECT row_to_json(b)::text FROM model_budgets b WHERE work_item_uuid=$1`, b.WorkItem).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for sequence := 1; sequence <= 2; sequence++ {
		ref := modelReserve(t, worker, child, sequence)
		if d, err := worker.ReserveModelCall(ctx, child.ID, child.PodUID, sequence, modelTestPayload); err != nil || d.Reason != "existing" {
			t.Fatal("duplicate repair call charged again", d.Reason, err)
		}
		if d, err := worker.ClaimModelCall(ctx, ref); err != nil || !d.Invoke {
			t.Fatal("repair call did not claim once", err)
		}
		response := []byte(`{"choices":[{"message":{"role":"assistant","content":"fixture"}}]}`)
		if _, err := worker.FinishModelCall(ctx, ref, 200, response); err != nil {
			t.Fatal(err)
		}
		if _, err := worker.AcknowledgeModelResponse(ctx, ref, authority.Digest(response), 200, len(response)); err != nil {
			t.Fatal(err)
		}
	}
	if d, err := worker.ReserveModelCall(ctx, child.ID, child.PodUID, 3, modelTestPayload); err != nil || d.Reason != "budget-exhausted" {
		t.Fatal("repair exceeded derived allowance", d.Reason, err)
	}
	if d, err := worker.ReserveModelCall(ctx, parent.ID, parent.PodUID, 1, modelTestPayload); err != nil || d.Reason != "inactive" {
		t.Fatal("original allocation became active", d.Reason, err)
	}
	var after string
	if err := db.Pool.QueryRow(`SELECT row_to_json(b)::text FROM model_budgets b WHERE work_item_uuid=$1`, b.WorkItem).Scan(&after); err != nil || before != after {
		t.Fatal("repair mutated sealed original budget", err)
	}
}

func TestRepairLedgerRejectsSubstitutionAndStopsChildOnly(t *testing.T) {
	for _, mutation := range []string{
		`UPDATE execution_repairs SET child_request_id='execution-'||repeat('f',64) WHERE work_item_uuid=$1`,
		`UPDATE repair_model_budgets SET max_calls=4 WHERE work_item_uuid=$1`,
		`UPDATE model_allocations SET response_limit=512 WHERE id=$1`,
		`UPDATE model_budgets SET stop_reason='different' WHERE work_item_uuid=$1`,
	} {
		t.Run(strings.Fields(mutation)[1], func(t *testing.T) {
			db, b, _, child := repairLedgerFixture(t)
			var err error
			if strings.Contains(mutation, "model_allocations") {
				_, err = db.Pool.Exec(mutation, child.ID)
			} else {
				_, err = db.Pool.Exec(mutation, b.WorkItem)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.ReserveModelCall(context.Background(), child.ID, child.PodUID, 1, modelTestPayload); err == nil {
				t.Fatal("changed repair linkage was accepted")
			}
		})
	}
	db, b, _, child := repairLedgerFixture(t)
	wrong := NewID()
	if d, err := db.ReserveModelCall(context.Background(), child.ID, wrong, 1, modelTestPayload); err != nil || d.Reason != "pod-mismatch" {
		t.Fatal("wrong pod not rejected", err)
	}
	var state, reason, original string
	if err := db.Pool.QueryRow(`SELECT r.state,r.stop_reason,b.stop_reason FROM repair_model_budgets r JOIN model_budgets b USING(work_item_uuid) WHERE work_item_uuid=$1`, b.WorkItem).Scan(&state, &reason, &original); err != nil || state != "stopped" || reason != "pod-mismatch" || original != "candidate-captured" {
		t.Fatal("repair stop affected original ledger", err)
	}
}
