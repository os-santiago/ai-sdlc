CREATE TABLE provenance_handoffs (
 build_id text PRIMARY KEY REFERENCES built_records(request_id),
 command_id text NOT NULL UNIQUE CHECK(command_id ~ '^verify-[a-f0-9]{64}$'),
 payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE provenance_handoffs ENABLE ROW LEVEL SECURITY;
ALTER TABLE provenance_handoffs FORCE ROW LEVEL SECURITY;
CREATE POLICY provenance_handoff_read ON provenance_handoffs FOR SELECT USING(current_user='aisdlc_build_controller');
CREATE POLICY provenance_handoff_insert ON provenance_handoffs FOR INSERT WITH CHECK(current_user='aisdlc_build_controller');
REVOKE ALL ON provenance_handoffs FROM PUBLIC;
