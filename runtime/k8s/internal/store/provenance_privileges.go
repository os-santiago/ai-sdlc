package store

var provenancePrivileges = []string{
	`GRANT SELECT ON provenance_requests,provenance_records TO aisdlc_verifier`,
	`GRANT INSERT(id,build_id,payload_digest,payload,receipt,configuration,expires_at) ON provenance_requests TO aisdlc_verifier`,
	`GRANT UPDATE(state,claim_token,reason) ON provenance_requests TO aisdlc_verifier`,
	`GRANT INSERT(request_id,digest,proof) ON provenance_records TO aisdlc_verifier`,
	`GRANT SELECT ON merged_records,merge_requests,published_records,publication_requests TO aisdlc_verifier`,
	`GRANT SELECT(repository,issue_number,generation,content_key) ON intake_heads TO aisdlc_verifier`,
	`GRANT SELECT(repository,issue_number,generation,revision,config_digest,payload) ON intake_revisions TO aisdlc_verifier`,
	`GRANT SELECT(repository,config_digest,watermark) ON intake_cursors TO aisdlc_verifier`,
	`GRANT SELECT(repository,issue_number,generation,config_digest,authority_revision,content_key,observed_at) ON intake_observations TO aisdlc_verifier`,
	`GRANT SELECT(uuid,status) ON work_items TO aisdlc_verifier`,
}
