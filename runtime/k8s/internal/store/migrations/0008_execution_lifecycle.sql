-- Hidden controller prerequisite: no operational grants are provisioned.
CREATE TABLE execution_lifecycles (
    request_id text PRIMARY KEY REFERENCES execution_requests(id),
    claim_token uuid NOT NULL,
    claim_until timestamptz NOT NULL,
    binding_digest text NOT NULL CHECK (binding_digest ~ '^[a-f0-9]{64}$'),
    source_digest text NOT NULL CHECK (source_digest ~ '^[a-f0-9]{64}$'),
    source_bytes integer NOT NULL CHECK(source_bytes BETWEEN 1 AND 33554432),
    namespace text NOT NULL CHECK (namespace ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    pod_name text NOT NULL CHECK (pod_name ~ '^executor-[a-f0-9-]{36}-[0-9]{1,2}$'),
    config_name text NOT NULL CHECK (config_name ~ '^assignment-[a-f0-9]{32}$'),
    phase text NOT NULL DEFAULT 'reserved' CHECK (phase IN ('reserved','config-issued','config-ready','pod-issued','pod-ready','source-issued','source-ready','start-issued','start-ready','export-issued','export-ready')),
    config_uid uuid,
    pod_uid uuid UNIQUE,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE(namespace,pod_name)
);
CREATE TABLE execution_effects (
    request_id text NOT NULL REFERENCES execution_lifecycles(request_id),
    kind text NOT NULL CHECK(kind IN ('config','pod','source','start','export')),
    token uuid NOT NULL UNIQUE,
    result bytea CHECK(result IS NULL OR octet_length(result) BETWEEN 1 AND 2048),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    completed_at timestamptz,
    PRIMARY KEY(request_id,kind),
    CHECK ((result IS NULL) = (completed_at IS NULL))
);
ALTER TABLE execution_lifecycles ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_lifecycles FORCE ROW LEVEL SECURITY;
CREATE POLICY lifecycle_worker ON execution_lifecycles USING (current_user='aisdlc_worker') WITH CHECK (current_user='aisdlc_worker');
ALTER TABLE execution_effects ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_effects FORCE ROW LEVEL SECURITY;
CREATE POLICY effect_worker ON execution_effects USING (current_user='aisdlc_worker') WITH CHECK (current_user='aisdlc_worker');
