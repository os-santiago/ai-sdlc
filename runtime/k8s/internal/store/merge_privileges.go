package store

// The migrator owns explicit activation and revokes every optional column on
// disable. Only release-manager can operate this journal; facts are immutable.
var mergePrivileges = []string{
	`GRANT SELECT ON merge_requests,merge_effects,merged_records TO aisdlc_release_manager`,
	`GRANT INSERT(id,publication_id,record_digest,gate_digest,authority_revision,payload_digest,payload,receipt,expires_at,gate,policy) ON merge_requests TO aisdlc_release_manager`,
	`GRANT UPDATE(state,claim_token,claim_until,reason,proof,merged_at) ON merge_requests TO aisdlc_release_manager`,
	`GRANT INSERT(request_id,token,request_digest) ON merge_effects TO aisdlc_release_manager`,
	`GRANT UPDATE(receipt,receipt_digest,not_sent_at,completed_at) ON merge_effects TO aisdlc_release_manager`,
	`GRANT INSERT(merge_id,payload_digest,payload,receipt,gate,policy,proof_digest,proof,effect_digest,effect,authority_expires_at) ON merged_records TO aisdlc_release_manager`,
}
