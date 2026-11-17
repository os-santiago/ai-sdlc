package store

var terminalPrivileges = []string{
	`GRANT SELECT ON terminal_requests,terminal_intents,terminal_sends,terminal_receipts TO aisdlc_release_manager`,
	`GRANT INSERT(id,deployment_id,payload_digest,payload,receipt,configuration,plan,expires_at) ON terminal_requests TO aisdlc_release_manager`,
	`GRANT UPDATE(state,claim_token,reason,completed_at) ON terminal_requests TO aisdlc_release_manager`,
	`GRANT INSERT(request_id,step,claim_token,request,request_digest) ON terminal_intents TO aisdlc_release_manager`,
	`GRANT INSERT(request_id,step,claim_token) ON terminal_sends TO aisdlc_release_manager`,
	`GRANT INSERT(request_id,step,digest,receipt) ON terminal_receipts TO aisdlc_release_manager`,
}
