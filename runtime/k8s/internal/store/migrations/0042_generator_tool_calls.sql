-- Generator tool calls: one durable row per catalog tool run a generation
-- requests through the mailbox. The row is written before the run (request
-- digest only) and finished after it with the run's identity. No tool input,
-- candidate diff, summary body, log or credential is persisted; the summary is
-- recorded by digest only. A row grants no candidate, verification or delivery
-- authority: it is evidence of what ran on the generation's behalf.
CREATE TABLE tool_calls (
    allocation_id uuid NOT NULL REFERENCES model_allocations(id),
    sequence integer NOT NULL CHECK (sequence BETWEEN 1 AND 8),
    request_sha256 text NOT NULL CHECK (request_sha256 ~ '^[a-f0-9]{64}$'),
    state text NOT NULL DEFAULT 'started' CHECK (state IN ('started','passed','failed','error','abandoned')),
    pipeline_run text NOT NULL DEFAULT '' CHECK (pipeline_run = '' OR pipeline_run ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'),
    revision text NOT NULL DEFAULT '' CHECK (revision = '' OR revision ~ '^[a-f0-9]{40}$'),
    tree text NOT NULL DEFAULT '' CHECK (tree = '' OR tree ~ '^[a-f0-9]{40}$'),
    summary_sha256 text NOT NULL DEFAULT '' CHECK (summary_sha256 = '' OR summary_sha256 ~ '^[a-f0-9]{64}$'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    finished_at timestamptz,
    PRIMARY KEY (allocation_id, sequence),
    -- Only a started call is unfinished.
    CHECK ((state = 'started') = (finished_at IS NULL)),
    -- A started call (not run yet, or run outcome unknown) and an abandoned
    -- call (recovery found it started) carry no run identity.
    CHECK (state NOT IN ('started','abandoned') OR (pipeline_run = '' AND revision = '' AND tree = '' AND summary_sha256 = '')),
    -- A decided run names its PipelineRun, base revision and summary digest.
    -- The tree exists only when a candidate diff was applied. An error may
    -- have happened before or after the PipelineRun existed.
    CHECK (state NOT IN ('passed','failed') OR (pipeline_run <> '' AND revision <> '' AND summary_sha256 <> ''))
);
-- At most one unfinished call per allocation: tool calls of a generation are
-- sequential, and recovery abandons the one left started.
CREATE UNIQUE INDEX tool_calls_one_started ON tool_calls(allocation_id) WHERE state = 'started';
ALTER TABLE tool_calls ENABLE ROW LEVEL SECURITY;
ALTER TABLE tool_calls FORCE ROW LEVEL SECURITY;
CREATE POLICY tool_call_worker ON tool_calls
    USING (current_user='aisdlc_worker') WITH CHECK (current_user='aisdlc_worker');
REVOKE ALL ON tool_calls FROM PUBLIC;
