CREATE TABLE promotion_handoffs (
 provenance_id text PRIMARY KEY REFERENCES provenance_records(request_id),
 command_id text NOT NULL UNIQUE CHECK(command_id ~ '^promote-[a-f0-9]{64}$'),
 payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE promotion_handoffs ENABLE ROW LEVEL SECURITY;
ALTER TABLE promotion_handoffs FORCE ROW LEVEL SECURITY;
CREATE POLICY promotion_handoff_read ON promotion_handoffs FOR SELECT USING(current_user='aisdlc_verifier');
CREATE POLICY promotion_handoff_insert ON promotion_handoffs FOR INSERT WITH CHECK(current_user='aisdlc_verifier');
REVOKE ALL ON promotion_handoffs FROM PUBLIC;
