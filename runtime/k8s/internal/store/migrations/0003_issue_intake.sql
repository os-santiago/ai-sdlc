CREATE TABLE intake_cursors (
    repository text NOT NULL,
    config_digest text NOT NULL,
    watermark timestamptz,
    next_poll timestamptz NOT NULL DEFAULT now(),
    failures integer NOT NULL DEFAULT 0 CHECK (failures BETWEEN 0 AND 20),
    lease_token uuid,
    lease_until timestamptz,
    PRIMARY KEY (repository, config_digest)
);
CREATE TABLE intake_revisions (
    repository text NOT NULL,
    issue_number integer NOT NULL CHECK (issue_number > 0),
    revision text NOT NULL,
    config_digest text NOT NULL,
    event_id text NOT NULL UNIQUE,
    payload bytea NOT NULL CHECK (octet_length(payload) BETWEEN 1 AND 1048576),
    observed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repository, issue_number, revision, config_digest)
);
