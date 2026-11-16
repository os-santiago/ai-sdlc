package store

var buildPrivileges = []string{
	`GRANT SELECT ON build_requests,build_slots,build_effects,merged_records,merge_requests,published_records,publication_requests TO aisdlc_build_controller`,
	`GRANT INSERT(id,merge_id,payload_digest,payload,receipt,inner_policy,outer_policy,expires_at) ON build_requests TO aisdlc_build_controller`,
	`GRANT UPDATE(state,claim_token,claim_until,reason) ON build_requests TO aisdlc_build_controller`,
	`GRANT INSERT(slot_id,request_id,slot_digest,slot) ON build_slots TO aisdlc_build_controller`,
	`GRANT INSERT(request_id,kind,token,request_digest,resource_name) ON build_effects TO aisdlc_build_controller`,
	`GRANT UPDATE(resource_uid,acknowledged_at) ON build_effects TO aisdlc_build_controller`,
	`GRANT SELECT(repository,issue_number,generation,content_key) ON intake_heads TO aisdlc_build_controller`,
	`GRANT SELECT(repository,issue_number,generation,revision,config_digest,payload) ON intake_revisions TO aisdlc_build_controller`,
	`GRANT SELECT(repository,config_digest,watermark) ON intake_cursors TO aisdlc_build_controller`,
	`GRANT SELECT(repository,issue_number,generation,config_digest,authority_revision,content_key,observed_at) ON intake_observations TO aisdlc_build_controller`,
	`GRANT SELECT(uuid,status) ON work_items TO aisdlc_build_controller`,
}
