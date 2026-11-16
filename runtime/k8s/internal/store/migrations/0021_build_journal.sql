-- Inactive until explicitly granted by a reviewed build installation.
CREATE TABLE build_requests (
 id text PRIMARY KEY CHECK(id ~ '^build-[a-f0-9]{64}$'),
 merge_id text NOT NULL UNIQUE REFERENCES merged_records(merge_id),
 payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
 payload bytea NOT NULL CHECK(octet_length(payload) BETWEEN 1 AND 16384),
 receipt bytea NOT NULL CHECK(octet_length(receipt) BETWEEN 1 AND 4096),
 inner_policy bytea NOT NULL CHECK(octet_length(inner_policy) BETWEEN 1 AND 32768),
 outer_policy bytea NOT NULL CHECK(octet_length(outer_policy) BETWEEN 1 AND 32768),
 expires_at timestamptz NOT NULL,
 state text NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','claimed','failed','built')),
 claim_token uuid,
 claim_until timestamptz,
 reason text NOT NULL DEFAULT '' CHECK(reason IN ('','expired','revoked','uncertain','rejected')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((claim_token IS NULL)=(claim_until IS NULL)),
 CHECK(state<>'queued' OR claim_token IS NULL),
 CHECK(state NOT IN ('claimed','built') OR claim_token IS NOT NULL),
 CHECK(claim_until IS NULL OR claim_until<=expires_at),
 CHECK((state='failed')=(reason<>''))
);
-- A consumed physical workspace is never made available by expiry, restart,
-- policy rotation, namespace replacement or cleanup. Reclamation is separate.
CREATE TABLE build_slots (
 slot_id text PRIMARY KEY CHECK(slot_id ~ '^slot-[a-f0-9]{64}$'),
 request_id text NOT NULL UNIQUE REFERENCES build_requests(id),
 slot_digest text NOT NULL CHECK(slot_digest ~ '^[a-f0-9]{64}$'),
 slot bytea NOT NULL CHECK(octet_length(slot) BETWEEN 1 AND 8192),
 allocated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE build_effects (
 request_id text NOT NULL REFERENCES build_requests(id),
 kind text NOT NULL CHECK(kind IN ('build','publish')),
 token uuid NOT NULL UNIQUE,
 request_digest text NOT NULL CHECK(request_digest ~ '^[a-f0-9]{64}$'),
 resource_name text NOT NULL CHECK(resource_name ~ '^ai-sdlc-(build|publish)-[a-f0-9]{32}$'),
 resource_uid uuid,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 acknowledged_at timestamptz,
 PRIMARY KEY(request_id,kind),
 CHECK((resource_uid IS NULL)=(acknowledged_at IS NULL))
);
ALTER TABLE build_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE build_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY build_request_owner ON build_requests USING(current_user='aisdlc_build_controller') WITH CHECK(current_user='aisdlc_build_controller');
ALTER TABLE build_slots ENABLE ROW LEVEL SECURITY;
ALTER TABLE build_slots FORCE ROW LEVEL SECURITY;
CREATE POLICY build_slot_owner ON build_slots FOR SELECT USING(current_user='aisdlc_build_controller');
CREATE POLICY build_slot_allocate ON build_slots FOR INSERT WITH CHECK(current_user='aisdlc_build_controller');
ALTER TABLE build_effects ENABLE ROW LEVEL SECURITY;
ALTER TABLE build_effects FORCE ROW LEVEL SECURITY;
CREATE POLICY build_effect_owner ON build_effects USING(current_user='aisdlc_build_controller') WITH CHECK(current_user='aisdlc_build_controller');
CREATE POLICY build_reader ON merged_records FOR SELECT USING(current_user='aisdlc_build_controller');
CREATE POLICY build_reader ON merge_requests FOR SELECT USING(current_user='aisdlc_build_controller');
CREATE POLICY build_reader ON published_records FOR SELECT USING(current_user='aisdlc_build_controller');
CREATE POLICY build_reader ON publication_requests FOR SELECT USING(current_user='aisdlc_build_controller');
CREATE POLICY build_reader ON intake_heads FOR SELECT USING(current_user='aisdlc_build_controller');
CREATE POLICY build_reader ON intake_revisions FOR SELECT USING(current_user='aisdlc_build_controller');
CREATE POLICY build_reader ON intake_cursors FOR SELECT USING(current_user='aisdlc_build_controller');
CREATE POLICY build_reader ON intake_observations FOR SELECT USING(current_user='aisdlc_build_controller');
CREATE POLICY build_reader ON work_items FOR SELECT USING(current_user='aisdlc_build_controller');
REVOKE ALL ON build_requests,build_slots,build_effects FROM PUBLIC;
