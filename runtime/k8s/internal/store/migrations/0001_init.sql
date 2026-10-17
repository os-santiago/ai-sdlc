-- 0001_init.sql — durable workflow state and the transactional outbox
-- (ADR-0002: "PostgreSQL stores durable workflow state and an outbox";
-- ADR-0005: closes ADR-0004's "wiring to a real event bus or durable
-- state store" gap).
--
-- One row per aggregate, idempotent on each table's natural key, so
-- redelivering the same upstream event through internal/outbox.Relay never
-- creates a duplicate decision.

CREATE TABLE IF NOT EXISTS admissions (
    repository      text        NOT NULL,
    issue_number    integer     NOT NULL,
    issue_revision  text        NOT NULL,
    decision        text        NOT NULL,
    risk            text        NOT NULL,
    policy_version  text        NOT NULL,
    reasons         jsonb       NOT NULL DEFAULT '[]',
    required_gates  jsonb       NOT NULL DEFAULT '[]',
    correlation_id  text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repository, issue_number, issue_revision)
);

CREATE TABLE IF NOT EXISTS plans (
    id              uuid        PRIMARY KEY,
    repository      text        NOT NULL,
    correlation_id  text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- work_items.id is the plan-scoped human key (internal/planning.WorkItem.ID,
-- referenced by DependsOn); work_items.uuid is the globally unique
-- identity carried in WorkItemReady.v1/ImplementationAttemptDecided.v1
-- (schema: workItemId, format uuid).
CREATE TABLE IF NOT EXISTS work_items (
    plan_id             uuid        NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
    id                  text        NOT NULL,
    uuid                uuid        NOT NULL UNIQUE,
    issue_numbers       jsonb       NOT NULL,
    acceptance_criteria jsonb       NOT NULL,
    depends_on          jsonb       NOT NULL DEFAULT '[]',
    status              text        NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (plan_id, id)
);

CREATE TABLE IF NOT EXISTS attempts (
    work_item_uuid      uuid        NOT NULL REFERENCES work_items(uuid) ON DELETE CASCADE,
    attempt_number      integer     NOT NULL,
    exit_code           integer     NOT NULL DEFAULT 0,
    timed_out           boolean     NOT NULL DEFAULT false,
    changed_files       jsonb       NOT NULL DEFAULT '[]',
    validation_passed   boolean     NOT NULL DEFAULT false,
    actionable_feedback jsonb       NOT NULL DEFAULT '[]',
    unsafe_or_ambiguous boolean     NOT NULL DEFAULT false,
    outcome             text        NOT NULL,
    reason              text        NOT NULL,
    reprompt            boolean     NOT NULL DEFAULT false,
    correlation_id      text        NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (work_item_uuid, attempt_number)
);

CREATE TABLE IF NOT EXISTS release_gates (
    repository            text        NOT NULL,
    pull_request           integer     NOT NULL,
    state                  text        NOT NULL,
    reasons                jsonb       NOT NULL DEFAULT '[]',
    remediation_attempts   integer     NOT NULL DEFAULT 0,
    correlation_id         text        NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repository, pull_request)
);

-- The transactional outbox (ADR-0002). Every component appends here in the
-- same transaction as its state write (internal/outbox.Append); relay.Relay
-- polls published_at IS NULL rows and delivers them.
CREATE TABLE IF NOT EXISTS outbox (
    id              uuid        PRIMARY KEY,
    aggregate_type  text        NOT NULL,
    aggregate_id    text        NOT NULL,
    event_type      text        NOT NULL,
    payload         jsonb       NOT NULL,
    correlation_id  text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    published_at    timestamptz
);

CREATE INDEX IF NOT EXISTS outbox_unpublished_idx ON outbox (created_at) WHERE published_at IS NULL;
