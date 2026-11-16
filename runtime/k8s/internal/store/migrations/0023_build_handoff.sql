-- One immutable dispatch per merged record. Revisions and retries cannot mint
-- another build for the same merge; lost delivery ACKs use the signed outbox.
CREATE TABLE build_handoffs (
 merge_id text PRIMARY KEY REFERENCES merged_records(merge_id),
 command_id text NOT NULL UNIQUE CHECK(command_id ~ '^build-[a-f0-9]{64}$'),
 payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE build_handoffs ENABLE ROW LEVEL SECURITY;
ALTER TABLE build_handoffs FORCE ROW LEVEL SECURITY;
CREATE POLICY build_handoff_read ON build_handoffs FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY build_handoff_insert ON build_handoffs FOR INSERT WITH CHECK(current_user='aisdlc_release_manager');
REVOKE ALL ON build_handoffs FROM PUBLIC;
