-- Signed delivery is separate from the legacy unsigned outbox. Old rows must
-- never be upgraded into authenticated facts by a relay or migrator.
CREATE TABLE managed_component_roles (
    component text PRIMARY KEY,
    role_name text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE authorized_outbox (
    producer text NOT NULL,
    event_id text NOT NULL,
    target text NOT NULL,
    operation text NOT NULL,
    receipt jsonb NOT NULL,
    payload bytea NOT NULL CHECK (octet_length(payload) BETWEEN 1 AND 1048576),
    created_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 8),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    exhausted boolean NOT NULL DEFAULT false,
    last_error text NOT NULL DEFAULT '',
    PRIMARY KEY (producer, event_id)
);
CREATE INDEX authorized_outbox_pending ON authorized_outbox(next_attempt_at)
    WHERE published_at IS NULL AND NOT exhausted;

CREATE TABLE authorized_inbox (
    target text NOT NULL,
    producer text NOT NULL,
    event_id text NOT NULL,
    receipt_digest text NOT NULL,
    result bytea CHECK (octet_length(result) BETWEEN 1 AND 1048576),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (target, producer, event_id)
);
CREATE TABLE authorized_nonces (
    target text NOT NULL,
    caller text NOT NULL,
    key_id text NOT NULL,
    nonce text NOT NULL,
    valid_until timestamptz NOT NULL,
    PRIMARY KEY (target, caller, key_id, nonce)
);
CREATE INDEX authorized_nonces_expiry ON authorized_nonces(valid_until);

-- The deployment provisions dedicated LOGIN roles through store.ProvisionRoles.
-- Policies rely on PostgreSQL identity, never caller-set session variables.
ALTER TABLE authorized_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE authorized_outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY authorized_outbox_insert ON authorized_outbox FOR INSERT
    WITH CHECK (current_user = 'aisdlc_' || replace(producer, '-', '_'));
CREATE POLICY authorized_outbox_read ON authorized_outbox FOR SELECT
    USING (current_user = 'aisdlc_relay' OR current_user = 'aisdlc_' || replace(producer, '-', '_'));
CREATE POLICY authorized_outbox_transport ON authorized_outbox FOR UPDATE
    USING (current_user = 'aisdlc_relay') WITH CHECK (current_user = 'aisdlc_relay');

ALTER TABLE authorized_inbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE authorized_inbox FORCE ROW LEVEL SECURITY;
CREATE POLICY authorized_inbox_owner ON authorized_inbox
    USING (current_user = 'aisdlc_' || replace(target, '-', '_'))
    WITH CHECK (current_user = 'aisdlc_' || replace(target, '-', '_'));
ALTER TABLE authorized_nonces ENABLE ROW LEVEL SECURITY;
ALTER TABLE authorized_nonces FORCE ROW LEVEL SECURITY;
CREATE POLICY authorized_nonces_owner ON authorized_nonces
    USING (current_user = 'aisdlc_' || replace(target, '-', '_'))
    WITH CHECK (current_user = 'aisdlc_' || replace(target, '-', '_'));
