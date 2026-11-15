-- Inactive until the verifier runtime profile explicitly grants privileges.
CREATE TABLE verification_handoffs (
    candidate_id text PRIMARY KEY REFERENCES execution_candidates(request_id),
    command_id text UNIQUE CHECK(command_id ~ '^verification-[a-f0-9]{64}$'),
    payload_digest text CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
    state text NOT NULL CHECK(state IN ('sent','rejected')),
    reason text NOT NULL DEFAULT '' CHECK(reason IN ('','unsupported-recipe','revoked')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK((state='sent')=(command_id IS NOT NULL AND payload_digest IS NOT NULL)),
    CHECK((state='rejected')=(reason<>'')),
    CHECK(state<>'rejected' OR (command_id IS NULL AND payload_digest IS NULL))
);
CREATE TABLE verification_requests (
    id text PRIMARY KEY CHECK(id ~ '^verification-[a-f0-9]{64}$'),
    candidate_id text NOT NULL UNIQUE REFERENCES execution_candidates(request_id),
    payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
    payload bytea NOT NULL CHECK(octet_length(payload) BETWEEN 1 AND 16384),
    receipt bytea NOT NULL CHECK(octet_length(receipt) BETWEEN 1 AND 4096),
    expires_at timestamptz NOT NULL,
    state text NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','claimed','passed','failed')),
    claim_token uuid,
    claim_until timestamptz,
    reason text NOT NULL DEFAULT '' CHECK(reason IN ('','expired','revoked','uncertain','rejected','comparison-failed')),
    cleanup_state text NOT NULL DEFAULT 'pending' CHECK(cleanup_state IN ('pending','complete','uncertain')),
    cleanup_attempts integer NOT NULL DEFAULT 0 CHECK(cleanup_attempts BETWEEN 0 AND 3),
    cleanup_next_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    proof bytea CHECK(proof IS NULL OR octet_length(proof) BETWEEN 1 AND 65536),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK((claim_token IS NULL)=(claim_until IS NULL)),
    CHECK(state<>'queued' OR claim_token IS NULL),
    CHECK(state NOT IN ('claimed','passed') OR claim_token IS NOT NULL),
    CHECK((state='passed')=(proof IS NOT NULL)),
    CHECK(state<>'passed' OR cleanup_state='complete'),
    CHECK((state='failed')=(reason<>''))
);
CREATE TABLE verification_effects (
    request_id text NOT NULL REFERENCES verification_requests(id),
    kind text NOT NULL CHECK(kind IN ('config','baseline','candidate','reference','tests')),
    token uuid NOT NULL UNIQUE,
    resource_name text NOT NULL CHECK(resource_name ~ '^verify-[a-f0-9]{32}-(inputs|baseline|candidate|reference|tests)$'),
    resource_uid uuid,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    completed_at timestamptz,
    PRIMARY KEY(request_id,kind),
    CHECK((resource_uid IS NULL)=(completed_at IS NULL))
);
ALTER TABLE verification_handoffs ENABLE ROW LEVEL SECURITY;
ALTER TABLE verification_handoffs FORCE ROW LEVEL SECURITY;
CREATE POLICY handoff_worker ON verification_handoffs USING(current_user='aisdlc_worker') WITH CHECK(current_user='aisdlc_worker');
CREATE POLICY handoff_verifier ON verification_handoffs FOR SELECT USING(current_user='aisdlc_verifier');
ALTER TABLE verification_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE verification_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY request_verifier ON verification_requests USING(current_user='aisdlc_verifier') WITH CHECK(current_user='aisdlc_verifier');
ALTER TABLE verification_effects ENABLE ROW LEVEL SECURITY;
ALTER TABLE verification_effects FORCE ROW LEVEL SECURITY;
CREATE POLICY effect_verifier ON verification_effects USING(current_user='aisdlc_verifier') WITH CHECK(current_user='aisdlc_verifier');
-- Current issue evidence must also be visible through the independent role.
CREATE POLICY verifier_work_reader ON work_items FOR SELECT USING(current_user='aisdlc_verifier');
CREATE POLICY verifier_plan_reader ON plans FOR SELECT USING(current_user='aisdlc_verifier');
CREATE POLICY verifier_intake_head_reader ON intake_heads FOR SELECT USING(current_user='aisdlc_verifier');
CREATE POLICY verifier_intake_revision_reader ON intake_revisions FOR SELECT USING(current_user='aisdlc_verifier');
CREATE POLICY verifier_intake_cursor_reader ON intake_cursors FOR SELECT USING(current_user='aisdlc_verifier');
CREATE POLICY verifier_intake_observation_reader ON intake_observations FOR SELECT USING(current_user='aisdlc_verifier');
CREATE POLICY verifier_admission_head_reader ON admission_heads FOR SELECT USING(current_user='aisdlc_verifier');
CREATE POLICY verifier_planning_head_reader ON planning_heads FOR SELECT USING(current_user='aisdlc_verifier');
CREATE POLICY verifier_admission_reader ON admissions FOR SELECT USING(current_user='aisdlc_verifier');
REVOKE ALL ON verification_handoffs,verification_requests,verification_effects FROM PUBLIC;
