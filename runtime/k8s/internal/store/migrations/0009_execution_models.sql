-- Immutable linkage only. Operational roles receive no new grants.
CREATE TABLE execution_models (
    request_id text PRIMARY KEY REFERENCES execution_lifecycles(request_id),
    allocation_id uuid NOT NULL UNIQUE REFERENCES model_allocations(id),
    config_digest text NOT NULL CHECK(config_digest ~ '^[a-f0-9]{64}$'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE execution_models ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_models FORCE ROW LEVEL SECURITY;
CREATE POLICY execution_model_worker ON execution_models USING (current_user='aisdlc_worker') WITH CHECK (current_user='aisdlc_worker');
