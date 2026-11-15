-- Inactive: no installation profile grants access to this independent journal.
-- Generation model reservations remain sealed and are never reused for review.
CREATE TABLE review_runs (
    id text PRIMARY KEY CHECK(id ~ '^review-[a-f0-9]{64}$'),
    verification_id text NOT NULL UNIQUE REFERENCES verification_requests(id),
    candidate_id text NOT NULL UNIQUE REFERENCES execution_candidates(request_id),
    work_item_uuid uuid NOT NULL REFERENCES work_items(uuid),
    behavior_digest text NOT NULL CHECK(behavior_digest ~ '^[a-f0-9]{64}$'),
    profile_digest text NOT NULL CHECK(profile_digest ~ '^[a-f0-9]{64}$'),
    configuration_digest text NOT NULL CHECK(configuration_digest ~ '^[a-f0-9]{64}$'),
    assignment_digest text NOT NULL CHECK(assignment_digest ~ '^[a-f0-9]{64}$'),
    expires_at timestamptz NOT NULL,
    state text NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','claimed','passed','failed')),
    claim_token uuid,
    claim_until timestamptz,
    reason text NOT NULL DEFAULT '' CHECK(reason IN ('','expired','revoked','uncertain','rejected','request-changes')),
    cleanup_state text NOT NULL DEFAULT 'pending' CHECK(cleanup_state IN ('pending','complete','uncertain')),
    cleanup_attempts integer NOT NULL DEFAULT 0 CHECK(cleanup_attempts BETWEEN 0 AND 3),
    cleanup_next_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    reserved_calls integer NOT NULL DEFAULT 0 CHECK(reserved_calls BETWEEN 0 AND 8),
    reserved_input_bytes integer NOT NULL DEFAULT 0 CHECK(reserved_input_bytes BETWEEN 0 AND 1048576),
    proof bytea CHECK(proof IS NULL OR octet_length(proof) BETWEEN 1 AND 65536),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK((claim_token IS NULL)=(claim_until IS NULL)),
    CHECK(state<>'queued' OR claim_token IS NULL),
    CHECK(state NOT IN ('claimed','passed') OR claim_token IS NOT NULL),
    CHECK(claim_until IS NULL OR claim_until<=expires_at),
    CHECK((state='passed')=(proof IS NOT NULL)),
    CHECK(state<>'passed' OR cleanup_state='complete'),
    CHECK((state='failed')=(reason<>''))
);
CREATE TABLE review_effects (
    run_id text NOT NULL REFERENCES review_runs(id),
    kind text NOT NULL CHECK(kind IN ('config','pod','import','start','export')),
    token uuid NOT NULL UNIQUE,
    resource_name text NOT NULL CHECK(length(resource_name) BETWEEN 1 AND 128),
    resource_uid uuid,
    digest text CHECK(digest ~ '^[a-f0-9]{64}$'),
    not_sent_at timestamptz,
    completed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(run_id,kind),
    CHECK((resource_uid IS NULL)=(completed_at IS NULL)),
    CHECK(not_sent_at IS NULL OR (completed_at IS NULL AND resource_uid IS NULL AND digest IS NULL)),
    CHECK(kind IN ('config','pod','start') OR completed_at IS NULL OR digest IS NOT NULL)
);
CREATE TABLE review_model_calls (
    run_id text NOT NULL REFERENCES review_runs(id),
    sequence integer NOT NULL CHECK(sequence BETWEEN 1 AND 8),
    token uuid NOT NULL UNIQUE,
    pod_uid uuid NOT NULL,
    request_digest text NOT NULL CHECK(request_digest ~ '^[a-f0-9]{64}$'),
    input_bytes integer NOT NULL CHECK(input_bytes BETWEEN 1 AND 131072),
    state text NOT NULL DEFAULT 'started' CHECK(state IN ('started','completed','handed-off','uncertain')),
    response_digest text CHECK(response_digest ~ '^[a-f0-9]{64}$'),
    output_bytes integer CHECK(output_bytes BETWEEN 1 AND 32768),
    observation bytea CHECK(observation IS NULL OR octet_length(observation) BETWEEN 1 AND 4096),
    handed_off_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(run_id,sequence),
    CHECK((response_digest IS NULL)=(output_bytes IS NULL)),
    CHECK((response_digest IS NULL)=(observation IS NULL)),
    CHECK(state NOT IN ('completed','handed-off') OR response_digest IS NOT NULL),
    CHECK((state='handed-off')=(handed_off_at IS NOT NULL))
);
ALTER TABLE review_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE review_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY independent_review_owner ON review_runs USING(current_user='aisdlc_verifier') WITH CHECK(current_user='aisdlc_verifier');
ALTER TABLE review_effects ENABLE ROW LEVEL SECURITY;
ALTER TABLE review_effects FORCE ROW LEVEL SECURITY;
CREATE POLICY independent_review_effect_owner ON review_effects USING(current_user='aisdlc_verifier') WITH CHECK(current_user='aisdlc_verifier');
ALTER TABLE review_model_calls ENABLE ROW LEVEL SECURITY;
ALTER TABLE review_model_calls FORCE ROW LEVEL SECURITY;
CREATE POLICY independent_review_model_owner ON review_model_calls USING(current_user='aisdlc_verifier') WITH CHECK(current_user='aisdlc_verifier');
REVOKE ALL ON review_runs,review_effects,review_model_calls FROM PUBLIC;
