package store

var promotionPrivileges = []string{
	`GRANT SELECT ON promotion_requests,promotion_records TO aisdlc_release_manager`,
	`GRANT INSERT(id,provenance_id,payload_digest,payload,receipt,configuration,expires_at) ON promotion_requests TO aisdlc_release_manager`,
	`GRANT UPDATE(state,claim_token,reason) ON promotion_requests TO aisdlc_release_manager`,
	`GRANT INSERT(request_id,digest,plan) ON promotion_records TO aisdlc_release_manager`,
	`GRANT SELECT ON provenance_requests,provenance_records,build_requests,built_records,build_slots,build_plans,build_contexts,build_effects,build_observations TO aisdlc_release_manager`,
}
