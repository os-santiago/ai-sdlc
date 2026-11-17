package store

// Column grants prevent the reviewer from rewriting queued source bindings or
// existing model request identities. Generation and publication remain separate.
var reviewPrivileges = []string{
	`GRANT SELECT ON review_feedback TO aisdlc_verifier`,
	`GRANT INSERT(review_id,decision,digest) ON review_feedback TO aisdlc_verifier`,
	`GRANT SELECT ON review_runs,review_effects,review_model_calls TO aisdlc_verifier`,
	`GRANT INSERT(id,verification_id,candidate_id,work_item_uuid,behavior_digest,profile_digest,configuration_digest,assignment_digest,expires_at) ON review_runs TO aisdlc_verifier`,
	`GRANT UPDATE(state,claim_token,claim_until,reason,cleanup_state,cleanup_attempts,cleanup_next_at,reserved_calls,reserved_input_bytes,proof) ON review_runs TO aisdlc_verifier`,
	`GRANT INSERT(run_id,kind,token,resource_name) ON review_effects TO aisdlc_verifier`,
	`GRANT UPDATE(resource_uid,digest,not_sent_at,completed_at) ON review_effects TO aisdlc_verifier`,
	`GRANT INSERT(run_id,sequence,token,pod_uid,request_digest,input_bytes) ON review_model_calls TO aisdlc_verifier`,
	`GRANT UPDATE(state,response_digest,output_bytes,observation,handed_off_at) ON review_model_calls TO aisdlc_verifier`,
}
