-- Bounded unvalidated artifact metadata. No operational grants are provisioned.
ALTER TABLE execution_lifecycles DROP CONSTRAINT execution_lifecycles_phase_check;
ALTER TABLE execution_lifecycles ADD CONSTRAINT execution_lifecycles_phase_check CHECK (
    phase IN ('reserved','config-issued','config-ready','pod-issued','pod-ready','source-issued','source-ready','start-issued','start-ready','export-issued','export-ready','candidate-captured')
);
CREATE TABLE execution_candidates (
    request_id text PRIMARY KEY REFERENCES execution_lifecycles(request_id),
    allocation_id uuid NOT NULL UNIQUE REFERENCES model_allocations(id),
    pod_uid uuid NOT NULL UNIQUE,
    source_sha text NOT NULL CHECK (source_sha ~ '^[a-f0-9]{40}$'),
    source_tree_sha256 text NOT NULL CHECK (source_tree_sha256 ~ '^[a-f0-9]{64}$'),
    candidate_tree_sha256 text NOT NULL CHECK (candidate_tree_sha256 ~ '^[a-f0-9]{64}$'),
    export_sha256 text NOT NULL CHECK (export_sha256 ~ '^[a-f0-9]{64}$'),
    export_bytes integer NOT NULL CHECK (export_bytes BETWEEN 1 AND 33554432),
    delta_sha256 text NOT NULL CHECK (delta_sha256 ~ '^[a-f0-9]{64}$'),
    delta bytea NOT NULL CHECK (octet_length(delta) BETWEEN 1 AND 2097152),
    changes integer NOT NULL CHECK (changes BETWEEN 1 AND 64),
    model_calls integer NOT NULL CHECK (model_calls BETWEEN 1 AND 128),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (source_tree_sha256 <> candidate_tree_sha256)
);
ALTER TABLE execution_candidates ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_candidates FORCE ROW LEVEL SECURITY;
CREATE POLICY candidate_worker ON execution_candidates
    USING (current_user='aisdlc_worker') WITH CHECK (current_user='aisdlc_worker');
REVOKE ALL ON execution_candidates FROM PUBLIC;
