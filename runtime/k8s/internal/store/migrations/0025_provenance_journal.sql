-- Inactive: explicit provenance grants are separate from build capability.
CREATE TABLE provenance_requests (
 id text PRIMARY KEY CHECK(id ~ '^verify-[a-f0-9]{64}$'),
 build_id text NOT NULL UNIQUE REFERENCES built_records(request_id),
 payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
 payload bytea NOT NULL CHECK(octet_length(payload) BETWEEN 1 AND 8192),
 receipt bytea NOT NULL CHECK(octet_length(receipt) BETWEEN 1 AND 4096),
 configuration bytea NOT NULL CHECK(octet_length(configuration) BETWEEN 1 AND 196608),
 expires_at timestamptz NOT NULL,
 state text NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','claimed','failed','verified')),
 claim_token uuid,
 reason text NOT NULL DEFAULT '' CHECK(reason IN ('','expired','revoked','uncertain','rejected')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((state='queued' AND claim_token IS NULL) OR state='failed' OR (state IN ('claimed','verified') AND claim_token IS NOT NULL)),
 CHECK((state='failed')=(reason<>''))
);
CREATE TABLE provenance_records (
 request_id text PRIMARY KEY REFERENCES provenance_requests(id),
 digest text NOT NULL CHECK(digest ~ '^[a-f0-9]{64}$'),
 proof bytea NOT NULL CHECK(octet_length(proof) BETWEEN 1 AND 8192),
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE provenance_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE provenance_requests FORCE ROW LEVEL SECURITY;
ALTER TABLE provenance_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE provenance_records FORCE ROW LEVEL SECURITY;
CREATE POLICY provenance_request_owner ON provenance_requests USING(current_user='aisdlc_verifier') WITH CHECK(current_user='aisdlc_verifier');
CREATE POLICY provenance_record_read ON provenance_records FOR SELECT USING(current_user='aisdlc_verifier');
CREATE POLICY provenance_record_insert ON provenance_records FOR INSERT WITH CHECK(current_user='aisdlc_verifier' AND EXISTS(
 SELECT 1 FROM provenance_requests r WHERE r.id=provenance_records.request_id AND r.state='claimed' AND r.expires_at>clock_timestamp()
 AND provenance_records.recorded_at>=r.created_at AND provenance_records.recorded_at<r.expires_at));
CREATE POLICY provenance_reader ON merged_records FOR SELECT USING(current_user='aisdlc_verifier');
CREATE POLICY provenance_reader ON merge_requests FOR SELECT USING(current_user='aisdlc_verifier');
CREATE POLICY provenance_reader ON published_records FOR SELECT USING(current_user='aisdlc_verifier');
CREATE POLICY provenance_reader ON publication_requests FOR SELECT USING(current_user='aisdlc_verifier');
-- Existing verifier intake/work-item RLS remains unchanged.
REVOKE ALL ON provenance_requests,provenance_records FROM PUBLIC;
