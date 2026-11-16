package store

// Installed only through the explicit publication capability.
var publicationPrivileges = []string{
	`GRANT SELECT ON published_records TO aisdlc_release_manager`,
	`GRANT INSERT(publication_id,payload_digest,payload,receipt,proof_digest,proof,effects_digest,effects,authority_expires_at) ON published_records TO aisdlc_release_manager`,
	`GRANT SELECT ON execution_candidates,execution_requests,execution_lifecycles,execution_models,model_allocations,model_budgets TO aisdlc_release_manager`,
	`GRANT SELECT(id,repository,correlation_id) ON plans TO aisdlc_release_manager`,
	`GRANT SELECT(uuid,plan_id,status,issue_numbers,acceptance_criteria,depends_on) ON work_items TO aisdlc_release_manager`,
	`GRANT SELECT(repository,issue_number,generation,content_key) ON intake_heads TO aisdlc_release_manager`,
	`GRANT SELECT(repository,issue_number,generation,event_id,revision,config_digest,payload) ON intake_revisions TO aisdlc_release_manager`,
	`GRANT SELECT(repository,config_digest,watermark) ON intake_cursors TO aisdlc_release_manager`,
	`GRANT SELECT(repository,issue_number,generation,config_digest,authority_revision,content_key,observed_at) ON intake_observations TO aisdlc_release_manager`,
	`GRANT SELECT(repository,issue_number,generation,event_id,payload) ON admission_heads,planning_heads TO aisdlc_release_manager`,
	`GRANT SELECT(repository,issue_number,issue_revision,decision,risk,policy_version,correlation_id,reasons,required_gates) ON admissions TO aisdlc_release_manager`,
	`GRANT SELECT ON publication_handoffs,review_runs,verification_requests,publication_requests,publication_effects TO aisdlc_release_manager`,
	`GRANT INSERT(id,review_id,verification_id,candidate_id,repository,payload_digest,payload,receipt,expires_at) ON publication_requests TO aisdlc_release_manager`,
	`GRANT UPDATE(state,claim_token,claim_until,plan_digest,commit_time,reason,proof) ON publication_requests TO aisdlc_release_manager`,
	`GRANT INSERT(request_id,kind,token,request_digest) ON publication_effects TO aisdlc_release_manager`,
	`GRANT UPDATE(receipt,receipt_digest,completed_at,not_sent_at) ON publication_effects TO aisdlc_release_manager`,
}
