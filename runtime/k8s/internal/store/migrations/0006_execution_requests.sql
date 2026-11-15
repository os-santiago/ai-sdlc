-- Hidden controller prerequisite. ProvisionRoles intentionally grants no
-- operational access until the complete reconciler is reviewed and installed.
CREATE TABLE execution_requests (
    id text PRIMARY KEY CHECK (id ~ '^execution-[a-f0-9]{64}$'),
    work_item_uuid uuid NOT NULL REFERENCES work_items(uuid),
    attempt integer NOT NULL CHECK (attempt BETWEEN 1 AND 16),
    repository text NOT NULL,
    authority_revision text NOT NULL CHECK (authority_revision ~ '^[a-f0-9]{64}$'),
    payload_digest text NOT NULL CHECK (payload_digest ~ '^[a-f0-9]{64}$'),
    payload bytea NOT NULL CHECK (octet_length(payload) BETWEEN 1 AND 16384),
    receipt bytea NOT NULL CHECK (octet_length(receipt) BETWEEN 1 AND 4096),
    expires_at timestamptz NOT NULL,
    state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','claimed','stopped')),
    claim_token uuid,
    claim_until timestamptz,
    stop_reason text NOT NULL DEFAULT '' CHECK (stop_reason IN ('','aborted','expired','authority-revoked','uncertain')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (work_item_uuid, attempt),
    CHECK ((claim_token IS NULL) = (claim_until IS NULL)),
    CHECK (state <> 'queued' OR claim_token IS NULL),
    CHECK (state <> 'claimed' OR claim_token IS NOT NULL),
    CHECK ((state = 'stopped') = (stop_reason <> ''))
);
CREATE UNIQUE INDEX execution_one_active_work_item ON execution_requests(work_item_uuid)
    WHERE state IN ('queued','claimed');
ALTER TABLE execution_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY execution_worker ON execution_requests
    USING (current_user = 'aisdlc_worker')
    WITH CHECK (current_user = 'aisdlc_worker');
