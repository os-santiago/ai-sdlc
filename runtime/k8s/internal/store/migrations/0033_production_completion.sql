CREATE TABLE production_handoffs(
 terminal_id text PRIMARY KEY REFERENCES terminal_requests(id),
 command_id text NOT NULL UNIQUE CHECK(command_id ~ '^production-[a-f0-9]{64}$'),
 payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE production_records(
 terminal_id text PRIMARY KEY REFERENCES terminal_requests(id),
 work_item_id uuid NOT NULL UNIQUE REFERENCES work_items(uuid),
 record_digest text NOT NULL CHECK(record_digest ~ '^[a-f0-9]{64}$'),
 command_id text NOT NULL UNIQUE CHECK(command_id ~ '^production-[a-f0-9]{64}$'),
 payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE production_handoffs ENABLE ROW LEVEL SECURITY;
ALTER TABLE production_handoffs FORCE ROW LEVEL SECURITY;
CREATE POLICY production_handoff_read ON production_handoffs FOR SELECT USING(current_user IN('aisdlc_release_manager','aisdlc_orchestrator'));
CREATE POLICY production_handoff_insert ON production_handoffs FOR INSERT WITH CHECK(current_user='aisdlc_release_manager');
ALTER TABLE production_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE production_records FORCE ROW LEVEL SECURITY;
CREATE POLICY production_fact_read ON production_records FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_fact_insert ON production_records FOR INSERT WITH CHECK(current_user='aisdlc_orchestrator');
REVOKE ALL ON production_handoffs,production_records FROM PUBLIC;
CREATE POLICY production_history_reader ON terminal_requests FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON terminal_intents FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON terminal_sends FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON terminal_receipts FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON deployment_requests FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON deployment_intents FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON deployment_sends FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON deployment_receipts FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON deployment_health FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON promotion_requests FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON promotion_records FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON provenance_requests FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON provenance_records FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON build_requests FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON built_records FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON build_slots FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON build_plans FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON build_contexts FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON build_effects FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON build_observations FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON merged_records FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON merge_requests FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON published_records FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY production_history_reader ON publication_requests FOR SELECT USING(current_user='aisdlc_orchestrator');
