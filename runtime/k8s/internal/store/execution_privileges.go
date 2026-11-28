package store

// Immutable command/candidate columns cannot be updated. Observation rows are
// read-only: the worker cannot manufacture eligibility or refresh its own lease.
var executionPrivileges = []string{
	`GRANT SELECT ON execution_repairs,repair_model_budgets,review_repair_handoffs,review_feedback TO aisdlc_worker`,
	`GRANT UPDATE(reserved_calls,reserved_input_bytes,reserved_output_bytes,state,stop_reason) ON repair_model_budgets TO aisdlc_worker`,
	`GRANT SELECT ON execution_failures TO aisdlc_worker`,
	`GRANT INSERT(request_id,pod_uid,container_id,exit_code) ON execution_failures TO aisdlc_worker`,
	`GRANT SELECT ON execution_requests TO aisdlc_worker`,
	`GRANT INSERT(id,work_item_uuid,attempt,repository,authority_revision,payload_digest,payload,receipt,expires_at) ON execution_requests TO aisdlc_worker`,
	`GRANT UPDATE(state,claim_token,claim_until,stop_reason,updated_at) ON execution_requests TO aisdlc_worker`,
	`GRANT SELECT,INSERT ON execution_lifecycles,execution_effects,execution_models,execution_candidates,model_budgets,model_allocations,model_calls TO aisdlc_worker`,
	`GRANT UPDATE(phase,config_uid,pod_uid,updated_at,cleanup_state,cleanup_attempts,cleanup_after) ON execution_lifecycles TO aisdlc_worker`,
	`GRANT UPDATE(result,completed_at) ON execution_effects TO aisdlc_worker`,
	`GRANT UPDATE(status,updated_at) ON work_items TO aisdlc_worker`,
	`GRANT UPDATE(reserved_calls,reserved_input_bytes,reserved_output_bytes,state,stop_reason) ON model_budgets TO aisdlc_worker`,
	`GRANT UPDATE(reserved_calls,reserved_input_bytes,reserved_output_bytes) ON model_allocations TO aisdlc_worker`,
	`GRANT UPDATE(state,output_bytes,response_sha256,response_status,response_handed_off_at) ON model_calls TO aisdlc_worker`,
	// The generator tool journal: begin inserts the request digest only;
	// finish (and recovery) record the outcome. Keys and digests are immutable.
	`GRANT SELECT ON tool_calls TO aisdlc_worker`,
	`GRANT INSERT(allocation_id,sequence,request_sha256) ON tool_calls TO aisdlc_worker`,
	`GRANT UPDATE(state,pipeline_run,revision,tree,summary_sha256,finished_at) ON tool_calls TO aisdlc_worker`,
	`GRANT SELECT(id,repository,correlation_id) ON plans TO aisdlc_worker`,
	`GRANT SELECT(uuid,plan_id,status,issue_numbers,acceptance_criteria,depends_on) ON work_items TO aisdlc_worker`,
	`GRANT SELECT(repository,issue_number,generation,content_key) ON intake_heads TO aisdlc_worker`,
	`GRANT SELECT(repository,issue_number,generation,event_id,revision,config_digest,payload) ON intake_revisions TO aisdlc_worker`,
	`GRANT SELECT(repository,config_digest,watermark) ON intake_cursors TO aisdlc_worker`,
	`GRANT SELECT(repository,issue_number,generation,config_digest,authority_revision,content_key,observed_at) ON intake_observations TO aisdlc_worker`,
	`GRANT SELECT(repository,issue_number,generation,event_id,payload) ON admission_heads,planning_heads TO aisdlc_worker`,
	`GRANT SELECT(repository,issue_number,issue_revision,decision,risk,policy_version,correlation_id,reasons,required_gates) ON admissions TO aisdlc_worker`,
}
