-- Diagnostic observations grant no candidate, retry or delivery authority.
CREATE TABLE execution_failures (
    request_id text PRIMARY KEY REFERENCES execution_lifecycles(request_id),
    pod_uid uuid NOT NULL,
    container_id text NOT NULL CHECK (container_id ~ '^[a-z0-9-]{1,32}://[a-f0-9]{32,128}$'),
    exit_code integer NOT NULL CHECK (exit_code BETWEEN 1 AND 255),
    observed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE execution_failures ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_failures FORCE ROW LEVEL SECURITY;
CREATE POLICY execution_failure_worker ON execution_failures
    USING (current_user='aisdlc_worker') WITH CHECK (current_user='aisdlc_worker');
REVOKE ALL ON execution_failures FROM PUBLIC;
