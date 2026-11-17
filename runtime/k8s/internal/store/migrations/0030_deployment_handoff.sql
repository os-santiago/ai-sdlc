CREATE TABLE deployment_handoffs(
 promotion_id text PRIMARY KEY REFERENCES promotion_records(request_id),
 command_id text NOT NULL UNIQUE CHECK(command_id ~ '^deploy-[a-f0-9]{64}$'),
 payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE deployment_handoffs ENABLE ROW LEVEL SECURITY;
ALTER TABLE deployment_handoffs FORCE ROW LEVEL SECURITY;
CREATE POLICY deployment_handoff_read ON deployment_handoffs FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY deployment_handoff_insert ON deployment_handoffs FOR INSERT WITH CHECK(current_user='aisdlc_release_manager');
REVOKE ALL ON deployment_handoffs FROM PUBLIC;
