-- Inactive: promotion requires an explicit role-provisioning opt-in.
CREATE TABLE promotion_requests (
 id text PRIMARY KEY CHECK(id ~ '^promote-[a-f0-9]{64}$'),
 provenance_id text NOT NULL UNIQUE REFERENCES provenance_records(request_id),
 payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
 payload bytea NOT NULL CHECK(octet_length(payload) BETWEEN 1 AND 16384),
 receipt bytea NOT NULL CHECK(octet_length(receipt) BETWEEN 1 AND 4096),
 configuration bytea NOT NULL CHECK(octet_length(configuration) BETWEEN 1 AND 327680),
 expires_at timestamptz NOT NULL,
 state text NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','claimed','failed','planned')),
 claim_token uuid,
 reason text NOT NULL DEFAULT '' CHECK(reason IN ('','expired','revoked','uncertain','rejected')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((state='queued' AND claim_token IS NULL) OR state='failed' OR (state IN ('claimed','planned') AND claim_token IS NOT NULL)),
 CHECK((state='failed')=(reason<>''))
);
CREATE TABLE promotion_records (
 request_id text PRIMARY KEY REFERENCES promotion_requests(id),
 digest text NOT NULL CHECK(digest ~ '^[a-f0-9]{64}$'),
 plan bytea NOT NULL CHECK(octet_length(plan) BETWEEN 1 AND 8192),
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE promotion_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE promotion_requests FORCE ROW LEVEL SECURITY;
ALTER TABLE promotion_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE promotion_records FORCE ROW LEVEL SECURITY;
CREATE POLICY promotion_request_owner ON promotion_requests USING(current_user='aisdlc_release_manager') WITH CHECK(current_user='aisdlc_release_manager');
CREATE POLICY promotion_record_read ON promotion_records FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY promotion_record_insert ON promotion_records FOR INSERT WITH CHECK(current_user='aisdlc_release_manager' AND EXISTS(
 SELECT 1 FROM promotion_requests r WHERE r.id=promotion_records.request_id AND r.state='claimed' AND r.expires_at>clock_timestamp()
 AND promotion_records.recorded_at>=r.created_at AND promotion_records.recorded_at<r.expires_at));
CREATE POLICY promotion_reader ON provenance_requests FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY promotion_reader ON provenance_records FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY promotion_reader ON build_requests FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY promotion_reader ON built_records FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY promotion_reader ON build_slots FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY promotion_reader ON build_plans FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY promotion_reader ON build_contexts FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY promotion_reader ON build_effects FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY promotion_reader ON build_observations FOR SELECT USING(current_user='aisdlc_release_manager');
REVOKE ALL ON promotion_requests,promotion_records FROM PUBLIC;
