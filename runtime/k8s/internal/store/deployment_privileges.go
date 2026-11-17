package store

var deploymentPrivileges = []string{
	`GRANT SELECT ON deployment_browser_intents,deployment_browser_sends,deployment_browser_pods,deployment_browser_evidence,deployment_browser_cleanups TO aisdlc_deployer`,
	`GRANT INSERT(id,deployment_id,phase,claim_token,request,request_digest,snapshot,rollout,profile_digest,pod_digest,expires_at,attempt_nonce) ON deployment_browser_intents TO aisdlc_deployer`,
	`GRANT INSERT(id,claim_token) ON deployment_browser_sends TO aisdlc_deployer`,
	`GRANT INSERT(id,pod_uid,not_sent) ON deployment_browser_pods TO aisdlc_deployer`,
	`GRANT INSERT(id,evidence,digest) ON deployment_browser_evidence TO aisdlc_deployer`,
	`GRANT INSERT(id,pod_uid) ON deployment_browser_cleanups TO aisdlc_deployer`,
	`GRANT SELECT ON deployment_requests,deployment_intents,deployment_sends,deployment_receipts,deployment_health TO aisdlc_deployer`,
	`GRANT INSERT(id,promotion_id,payload_digest,payload,receipt,configuration,expires_at) ON deployment_requests TO aisdlc_deployer`,
	`GRANT UPDATE(state,claim_token,reason) ON deployment_requests TO aisdlc_deployer`,
	`GRANT INSERT(request_id,kind,claim_token,before_snapshot,request_digest) ON deployment_intents TO aisdlc_deployer`,
	`GRANT INSERT(request_id,kind,claim_token) ON deployment_sends TO aisdlc_deployer`,
	`GRANT INSERT(request_id,kind,digest,receipt) ON deployment_receipts TO aisdlc_deployer`,
	`GRANT INSERT(request_id,kind,digest,health) ON deployment_health TO aisdlc_deployer`,
	`GRANT SELECT ON promotion_requests,promotion_records,provenance_requests,provenance_records,build_requests,built_records,build_slots,build_plans,build_contexts,build_effects,build_observations,merged_records,merge_requests,published_records,publication_requests TO aisdlc_deployer`,
	`GRANT SELECT(repository,issue_number,generation,content_key) ON intake_heads TO aisdlc_deployer`,
	`GRANT SELECT(repository,issue_number,generation,revision,config_digest,payload) ON intake_revisions TO aisdlc_deployer`,
	`GRANT SELECT(repository,config_digest,watermark) ON intake_cursors TO aisdlc_deployer`,
	`GRANT SELECT(repository,issue_number,generation,config_digest,authority_revision,content_key,observed_at) ON intake_observations TO aisdlc_deployer`,
	`GRANT SELECT(uuid,status) ON work_items TO aisdlc_deployer`,
}
