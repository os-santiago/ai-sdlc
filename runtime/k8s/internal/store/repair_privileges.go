package store

var repairOrchestratorPrivileges = []string{
	`GRANT SELECT ON review_repair_stops TO aisdlc_orchestrator`,
	`GRANT SELECT ON model_calls TO aisdlc_orchestrator`,
	`GRANT SELECT ON publication_handoffs TO aisdlc_orchestrator`,
	`GRANT SELECT(repository,issue_number,generation,content_key) ON intake_heads TO aisdlc_orchestrator`,
	`GRANT SELECT(repository,issue_number,generation,event_id,revision,config_digest,payload) ON intake_revisions TO aisdlc_orchestrator`,
	`GRANT SELECT(repository,config_digest,watermark) ON intake_cursors TO aisdlc_orchestrator`,
	`GRANT SELECT(repository,issue_number,generation,config_digest,authority_revision,content_key,observed_at) ON intake_observations TO aisdlc_orchestrator`,
	`GRANT SELECT(repository,issue_number,generation,event_id,payload) ON admission_heads TO aisdlc_orchestrator`,
	`GRANT SELECT(repository,issue_number,issue_revision,decision,risk,policy_version,correlation_id,reasons,required_gates) ON admissions TO aisdlc_orchestrator`,
	`GRANT EXECUTE ON FUNCTION public.lock_repair_parent_budget(uuid) TO aisdlc_orchestrator`,
	`GRANT SELECT ON review_repair_handoffs,execution_requests,execution_lifecycles,execution_models,execution_candidates,model_allocations,model_budgets,verification_requests,review_runs,review_effects,review_model_calls,review_feedback TO aisdlc_orchestrator`,
	`GRANT SELECT ON execution_repairs,repair_model_budgets TO aisdlc_orchestrator`,
	`GRANT INSERT(work_item_uuid,parent_review_id,parent_candidate_id,parent_allocation_id,child_request_id,child_allocation_id,handoff_digest,child_payload,expires_at) ON execution_repairs TO aisdlc_orchestrator`,
	`GRANT INSERT(work_item_uuid,allocation_id,max_calls,max_input_bytes,max_output_bytes,response_limit,expires_at) ON repair_model_budgets TO aisdlc_orchestrator`,
}

var repairVerifierPrivileges = []string{
	`GRANT SELECT ON review_repair_stops TO aisdlc_verifier`,
	`GRANT INSERT(review_id,work_item_uuid,payload,payload_digest) ON review_repair_stops TO aisdlc_verifier`,
	`GRANT SELECT ON model_calls TO aisdlc_verifier`,
	`GRANT SELECT ON review_repair_handoffs TO aisdlc_verifier`,
	`GRANT INSERT(review_id,work_item_uuid,outcome,payload,payload_digest) ON review_repair_handoffs TO aisdlc_verifier`,
}
