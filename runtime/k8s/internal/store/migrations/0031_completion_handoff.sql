CREATE TABLE completion_handoffs(
 deployment_id text PRIMARY KEY REFERENCES deployment_requests(id),
 command_id text NOT NULL UNIQUE CHECK(command_id ~ '^complete-[a-f0-9]{64}$'),
 payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE completion_handoffs ENABLE ROW LEVEL SECURITY;
ALTER TABLE completion_handoffs FORCE ROW LEVEL SECURITY;
CREATE POLICY completion_handoff_read ON completion_handoffs FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY completion_handoff_insert ON completion_handoffs FOR INSERT WITH CHECK(current_user='aisdlc_deployer');
CREATE POLICY completion_history_reader ON deployment_requests FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY completion_history_reader ON deployment_intents FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY completion_history_reader ON deployment_sends FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY completion_history_reader ON deployment_receipts FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY completion_history_reader ON deployment_health FOR SELECT USING(current_user='aisdlc_release_manager');
REVOKE ALL ON completion_handoffs FROM PUBLIC;
