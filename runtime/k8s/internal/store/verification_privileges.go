package store

// The verifier can read generation evidence and mutate only its own bounded
// verification ledger. It cannot rewrite candidate bytes or issue observations.
var verificationPrivileges = []string{
	`GRANT SELECT ON execution_repairs,repair_model_budgets TO aisdlc_verifier`,
	`GRANT SELECT ON verification_handoffs,execution_candidates,execution_requests,execution_lifecycles,execution_models,model_allocations,model_budgets TO aisdlc_verifier`,
	`GRANT SELECT ON verification_requests,verification_effects TO aisdlc_verifier`,
	`GRANT INSERT(id,candidate_id,payload_digest,payload,receipt,expires_at) ON verification_requests TO aisdlc_verifier`,
	`GRANT UPDATE(state,claim_token,claim_until,reason,cleanup_state,cleanup_attempts,cleanup_next_at,proof) ON verification_requests TO aisdlc_verifier`,
	`GRANT INSERT(request_id,kind,token,resource_name) ON verification_effects TO aisdlc_verifier`,
	`GRANT UPDATE(resource_uid,completed_at,not_sent_at) ON verification_effects TO aisdlc_verifier`,
	`GRANT SELECT(id,repository,correlation_id) ON plans TO aisdlc_verifier`,
	`GRANT SELECT(uuid,plan_id,status,issue_numbers,acceptance_criteria,depends_on) ON work_items TO aisdlc_verifier`,
	`GRANT SELECT(repository,issue_number,generation,content_key) ON intake_heads TO aisdlc_verifier`,
	`GRANT SELECT(repository,issue_number,generation,event_id,revision,config_digest,payload) ON intake_revisions TO aisdlc_verifier`,
	`GRANT SELECT(repository,config_digest,watermark) ON intake_cursors TO aisdlc_verifier`,
	`GRANT SELECT(repository,issue_number,generation,config_digest,authority_revision,content_key,observed_at) ON intake_observations TO aisdlc_verifier`,
	`GRANT SELECT(repository,issue_number,generation,event_id,payload) ON admission_heads,planning_heads TO aisdlc_verifier`,
	`GRANT SELECT(repository,issue_number,issue_revision,decision,risk,policy_version,correlation_id,reasons,required_gates) ON admissions TO aisdlc_verifier`,
}
