package store

var productionPrivileges = []string{
	`GRANT SELECT ON terminal_requests,terminal_intents,terminal_sends,terminal_receipts,deployment_requests,deployment_intents,deployment_sends,deployment_receipts,deployment_health,promotion_requests,promotion_records,provenance_requests,provenance_records,build_requests,built_records,build_slots,build_plans,build_contexts,build_effects,build_observations,merged_records,merge_requests,published_records,publication_requests TO aisdlc_orchestrator`,
	`GRANT SELECT ON production_handoffs,production_records TO aisdlc_orchestrator`,
	`GRANT INSERT(terminal_id,work_item_id,record_digest,command_id,payload_digest) ON production_records TO aisdlc_orchestrator`,
}
