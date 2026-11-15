-- Hidden controller-owned metadata. No component role receives access here.
-- No prompts, responses, credentials, prices or provider tokens are persisted.
CREATE TABLE model_budgets (
    work_item_uuid uuid PRIMARY KEY REFERENCES work_items(uuid),
    repository text NOT NULL CHECK (length(repository) BETWEEN 3 AND 256),
    source_sha text NOT NULL CHECK (source_sha ~ '^[a-f0-9]{40}$'),
    policy_digest text NOT NULL CHECK (policy_digest ~ '^[a-f0-9]{64}$'),
    model text NOT NULL CHECK (length(model) BETWEEN 1 AND 128),
    max_calls integer NOT NULL CHECK (max_calls BETWEEN 1 AND 128),
    max_input_bytes bigint NOT NULL CHECK (max_input_bytes BETWEEN 1 AND 16777216),
    max_output_bytes bigint NOT NULL CHECK (max_output_bytes BETWEEN 1 AND 33554432),
    reserved_calls integer NOT NULL DEFAULT 0 CHECK (reserved_calls BETWEEN 0 AND max_calls),
    reserved_input_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_input_bytes BETWEEN 0 AND max_input_bytes),
    reserved_output_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_output_bytes BETWEEN 0 AND max_output_bytes),
    expires_at timestamptz NOT NULL,
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','stopped')),
    stop_reason text NOT NULL DEFAULT '' CHECK (length(stop_reason) <= 64)
);
CREATE TABLE model_allocations (
    id uuid PRIMARY KEY,
    work_item_uuid uuid NOT NULL REFERENCES model_budgets(work_item_uuid),
    attempt integer NOT NULL CHECK (attempt BETWEEN 1 AND 16),
    pod_uid uuid NOT NULL UNIQUE,
    max_calls integer NOT NULL CHECK (max_calls BETWEEN 1 AND 32),
    max_input_bytes bigint NOT NULL CHECK (max_input_bytes BETWEEN 1 AND 4194304),
    max_output_bytes bigint NOT NULL CHECK (max_output_bytes BETWEEN 1 AND 8388608),
    response_limit integer NOT NULL CHECK (response_limit BETWEEN 1 AND 262144),
    reserved_calls integer NOT NULL DEFAULT 0 CHECK (reserved_calls BETWEEN 0 AND max_calls),
    reserved_input_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_input_bytes BETWEEN 0 AND max_input_bytes),
    reserved_output_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_output_bytes BETWEEN 0 AND max_output_bytes),
    expires_at timestamptz NOT NULL,
    UNIQUE(work_item_uuid, attempt)
);
CREATE TABLE model_calls (
    allocation_id uuid NOT NULL REFERENCES model_allocations(id),
    sequence integer NOT NULL CHECK (sequence BETWEEN 1 AND 32),
    request_sha256 text NOT NULL CHECK (request_sha256 ~ '^[a-f0-9]{64}$'),
    input_bytes integer NOT NULL CHECK (input_bytes BETWEEN 1 AND 131072),
    response_limit integer NOT NULL CHECK (response_limit BETWEEN 1 AND 262144),
    expires_at timestamptz NOT NULL,
    state text NOT NULL DEFAULT 'reserved' CHECK (state IN ('reserved','started','completed','ambiguous','stopped')),
    output_bytes integer CHECK (output_bytes BETWEEN 0 AND response_limit),
    response_sha256 text CHECK (response_sha256 ~ '^[a-f0-9]{64}$'),
    response_status integer CHECK (response_status IN (200,400,401,403,429,500,502,503,504)),
    PRIMARY KEY (allocation_id, sequence),
    CHECK ((state = 'completed') = (output_bytes IS NOT NULL AND response_sha256 IS NOT NULL AND response_status IS NOT NULL))
);
CREATE UNIQUE INDEX model_calls_one_pending ON model_calls(allocation_id) WHERE state IN ('reserved','started');
CREATE INDEX model_calls_expiring ON model_calls(expires_at) WHERE state IN ('reserved','started');
REVOKE ALL ON model_budgets, model_allocations, model_calls FROM PUBLIC;
