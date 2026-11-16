-- Inactive: publication requires an explicitly reviewed runtime/installation.
-- Neither model output nor a caller-supplied branch can enter this journal.
CREATE TABLE publication_handoffs (
    review_id text PRIMARY KEY REFERENCES review_runs(id),
    command_id text NOT NULL UNIQUE CHECK(command_id ~ '^publication-[a-f0-9]{64}$'),
    payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE publication_requests (
    id text PRIMARY KEY CHECK(id ~ '^publication-[a-f0-9]{64}$'),
    review_id text NOT NULL UNIQUE REFERENCES review_runs(id),
    verification_id text NOT NULL UNIQUE REFERENCES verification_requests(id),
    candidate_id text NOT NULL UNIQUE REFERENCES execution_candidates(request_id),
    repository text NOT NULL CHECK(repository='os-santiago/homedir'),
    payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
    payload bytea NOT NULL CHECK(octet_length(payload) BETWEEN 1 AND 16384),
    receipt bytea NOT NULL CHECK(octet_length(receipt) BETWEEN 1 AND 4096),
    expires_at timestamptz NOT NULL,
    state text NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','claimed','published','failed')),
    claim_token uuid,
    claim_until timestamptz,
    plan_digest text CHECK(plan_digest ~ '^[a-f0-9]{64}$'),
    commit_time timestamptz,
    reason text NOT NULL DEFAULT '' CHECK(reason IN ('','expired','revoked','uncertain','rejected')),
    proof bytea CHECK(proof IS NULL OR octet_length(proof) BETWEEN 1 AND 32768),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK((claim_token IS NULL)=(claim_until IS NULL)),
    CHECK((claim_token IS NULL)=(plan_digest IS NULL)),
    CHECK((claim_token IS NULL)=(commit_time IS NULL)),
    CHECK(state<>'queued' OR claim_token IS NULL),
    CHECK(state NOT IN ('claimed','published') OR claim_token IS NOT NULL),
    CHECK(claim_until IS NULL OR claim_until<=expires_at),
    CHECK((state='published')=(proof IS NOT NULL)),
    CHECK((state='failed')=(reason<>''))
);
CREATE TABLE publication_effects (
    request_id text NOT NULL REFERENCES publication_requests(id),
    kind text NOT NULL CHECK(kind IN ('tree','commit','branch','pull-request')),
    token uuid NOT NULL UNIQUE,
    request_digest text NOT NULL CHECK(request_digest ~ '^[a-f0-9]{64}$'),
    receipt bytea CHECK(receipt IS NULL OR octet_length(receipt) BETWEEN 1 AND 8192),
    receipt_digest text CHECK(receipt_digest ~ '^[a-f0-9]{64}$'),
    not_sent_at timestamptz,
    completed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(request_id,kind),
    CHECK((receipt IS NULL)=(receipt_digest IS NULL)),
    CHECK((receipt IS NULL)=(completed_at IS NULL)),
    CHECK(not_sent_at IS NULL OR completed_at IS NULL)
);
ALTER TABLE publication_handoffs ENABLE ROW LEVEL SECURITY;
ALTER TABLE publication_handoffs FORCE ROW LEVEL SECURITY;
CREATE POLICY publication_handoff_writer ON publication_handoffs USING(current_user='aisdlc_verifier') WITH CHECK(current_user='aisdlc_verifier');
CREATE POLICY publication_handoff_reader ON publication_handoffs FOR SELECT USING(current_user='aisdlc_release_manager');
ALTER TABLE publication_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE publication_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY publication_owner ON publication_requests USING(current_user='aisdlc_release_manager') WITH CHECK(current_user='aisdlc_release_manager');
ALTER TABLE publication_effects ENABLE ROW LEVEL SECURITY;
ALTER TABLE publication_effects FORCE ROW LEVEL SECURITY;
CREATE POLICY publication_effect_owner ON publication_effects USING(current_user='aisdlc_release_manager') WITH CHECK(current_user='aisdlc_release_manager');
CREATE POLICY publication_review_reader ON review_runs FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY publication_behavior_reader ON verification_requests FOR SELECT USING(current_user='aisdlc_release_manager');
REVOKE ALL ON publication_handoffs,publication_requests,publication_effects FROM PUBLIC;
