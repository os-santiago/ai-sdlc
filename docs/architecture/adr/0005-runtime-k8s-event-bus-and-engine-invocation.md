# ADR-0005: Durable state, transactional outbox and engine invocation for runtime/k8s

- **Status**: Proposed
- **Date**: 2026-10-16
- **Deciders**: see repo CODEOWNERS
- **Tracking**: follow-up to ADR-0004 ("Explicitly out of scope" items 1 and 2)

## Context

ADR-0004 ported four independently deployable, independently testable Go
components from `homedir-ai-sdlc/future-go/`, each exposing one pure,
synchronous `Evaluate(...)` HTTP endpoint. They were never wired together:
there was no durable state, no event bus, and `worker` evaluated attempts
without ever invoking an engine. That was deliberate — ADR-0004 shipped a
buildable/testable scaffold, not a running pipeline — but it means nothing
in `runtime/k8s` can process a real GitHub issue end-to-end yet.

This ADR closes both gaps so the runtime can be deployed to a real
OpenShift cluster and pointed at a real adopter repo (`os-santiago/homedir`,
per ADR-0001's pilot designation — see also the discussion thread on
self-hosting vs. piloting against an unrelated app, which concluded
`homedir` is the correct first target and `ai-sdlc` must not self-modify
its own release authority).

`homedir-ai-sdlc`'s own prior architecture decisions already answered the
state/bus question for this exact problem:

- ADR-0002 (`0002-gitops-and-saga-state.md`): *"PostgreSQL stores durable
  workflow state and an outbox... Multi-issue delivery uses
  dependency-aware sagas with compensating actions."*
- ADR-0001 (`0001-component-boundaries.md`): *"Package each as an
  independent OCI image and coordinate them through versioned
  contracts... Initially co-locate them in a pod."*

Neither ADR mandates a message broker (Kafka/NATS). This ADR follows that
precedent instead of introducing one.

## Decision

### 1. Durable state: one PostgreSQL database, four component schemas, one outbox

Add `internal/store`: a thin `database/sql` layer (`lib/pq`-free; uses
`jackc/pgx/v5/stdlib` as the only new third-party dependency) with
embedded SQL migrations (`go:embed`), applied idempotently on component
startup.

Tables (see `internal/store/migrations/0001_init.sql`):

- `admissions` — one row per `(repository, issue_number, issue_revision)`,
  the admission-controller's durable decision record. Idempotent upsert on
  that key: re-delivering the same normalized event is a no-op, not a
  duplicate admission.
- `plans` / `work_items` — the orchestrator's saga state (ADR-0002:
  "dependency-aware sagas with compensating actions"). `work_items.status`
  mirrors `internal/planning.Status`.
- `attempts` — one row per `(work_item_id, attempt_number)`, the worker's
  bounded-edit history. `cycle.Evaluate` remains a pure function; this
  table is the audit trail of what was fed to it and what it returned.
- `release_gates` — one row per `(repository, pull_request)`, the
  release-manager's latest `gates.Evaluate` snapshot and result.
- `outbox` — the transactional outbox (ADR-0002). Columns: `id` (uuid),
  `aggregate_type`, `aggregate_id`, `event_type`, `payload` (jsonb),
  `correlation_id`, `created_at`, `published_at` (nullable).

Every component writes its state row and its outbox row **in the same
database transaction** (`internal/store.Tx`), so a crash between "decision
made" and "event recorded" is impossible — the classic dual-write problem
an outbox exists to prevent.

Each component only needs `DATABASE_URL`; no component talks to another
component's tables directly (ADR-0001: "permission and failure boundaries
become explicit").

### 2. Event relay: a poller, not a broker

Add `internal/outbox` with two parts:

- `Append(ctx, tx, Record)` — called by a component inside its own
  write transaction.
- `Relay` — a small poller (new 5th binary, `components/relay`) that:
  1. `SELECT ... WHERE published_at IS NULL ORDER BY created_at FOR UPDATE
     SKIP LOCKED LIMIT N` (safe with multiple replicas).
  2. Looks up the destination URL for `event_type` in a static routing
     table (`internal/outbox.DefaultRoutes`, overridable via
     `AI_SDLC_RELAY_ROUTES`). Defaults address the other four components
     over `localhost`, since `deploy/kustomize/base/deployment.yaml`
     co-locates all five as containers in one pod (ADR-0001): no Service
     DNS hop, no `NetworkPolicy` egress rule needed for this traffic.
     `IssueAdmissionDecided.v1` and `ImplementationAttemptDecided.v1` →
     orchestrator's two new ingress routes; `WorkItemReady.v1` → worker's
     `/v1/attempts/dispatch`; `ReleaseGateDecided.v1` → orchestrator
     (routed, not yet consumed — see "Explicitly still out of scope").
  3. `POST`s the payload with the `idempotencyKey`/`eventId` as an
     idempotency header; the receiving component's own durable upsert
     (point 1) makes redelivery safe.
  4. Marks `published_at` on `2xx`; leaves the row unpublished (retried
     next poll, unbounded — matches ADR-0003's "the agent cannot edit its
     own release authority" posture of never silently dropping a
     decision) on failure.

This satisfies "an event bus" per ADR-0004's `AI_SDLC_EVENT_BUS_URL`
mention without adding Kafka/NATS/Redis as a new runtime dependency —
consistent with the rest of this runtime's zero-third-party-dependency
starting point (ADR-0004: "zero third-party dependencies" was true of the
pure domain packages; `pgx` is the one exception, scoped to `internal/store`
only, never imported by the pure `admission`/`cycle`/`planning`/`gates`
packages).

### 3. Engine invocation: a pluggable interface, stubbed for now

Add `internal/engine`:

```go
type Engine interface {
    Invoke(ctx context.Context, item WorkItemReady) (AttemptResult, error)
}
```

`StubEngine` is the only implementation shipped in this ADR: deterministic,
no subprocess, no network — returns a successful, zero-changed-files
`AttemptResult` so the pipeline is wire-testable end-to-end without
`scc`/model credentials. `worker` selects an `Engine` by
`AI_SDLC_ENGINE` env var (`stub` default; `scc` reserved for the real
subprocess implementation).

The real implementation (tracked as follow-up, not built here — no `scc`
binary or model credentials are available in this environment) will shell
out to the same headless contract the GHA runtime already uses
(`--prompt-file --audit-log --summary-file --no-commit`, ADR-0004 Decision
#2), parse `--summary-file`, and map it onto `cycle.Attempt`. The interface
boundary is drawn so that swap requires no change to `worker`'s HTTP layer,
persistence, or `cycle.Evaluate` call.

### 4. Postgres on the cluster: CloudNativePG

Add `deploy/kustomize/base/postgres-cluster.yaml`: a CloudNativePG
`Cluster` custom resource (1 instance for development, 3 for production via
overlay patch), **not** a hand-rolled `StatefulSet`. CloudNativePG is
chosen because:

- It is distributed via OpenShift's OperatorHub (no custom catalog source).
- It needs no privileged `SecurityContextConstraints` beyond `restricted-v2`.
- It owns backup/failover/minor-version upgrades, which a hand-rolled
  `StatefulSet` would otherwise need bespoke operational scripts for.

The operator itself is a cluster-scoped prerequisite (install once per
cluster, outside this Kustomize tree — documented in
`deploy/kustomize/base/README.md`). If an adopter already runs a different
operator or managed Postgres, `postgres-cluster.yaml` is skippable via the
overlay and `DATABASE_URL` is pointed at the existing instance instead —
the application code has no CloudNativePG-specific dependency, only a
`DATABASE_URL` env var.

## Consequences

### New
- `internal/store` (migrations + typed repositories for all four tables).
- `internal/outbox` (`Append` + `Relay`).
- `internal/engine` (`Engine` interface + `StubEngine`).
- `components/relay` (5th deployable binary; thin `main.go` wrapping
  `outbox.Relay`).
- Two new HTTP ingress routes, additive alongside each component's
  original ADR-0004 endpoint, which stays unchanged for dry runs/tests:
  - `orchestrator`: `POST /v1/plans/events/issue-admission-decided` and
    `POST /v1/plans/events/implementation-attempt-decided` — the actual
    saga-state transitions (`handleIssueAdmissionDecided`,
    `handleImplementationAttemptDecided`), both requiring `Deps.DB`.
  - `worker`: `POST /v1/attempts/dispatch` — runs the bounded-edit loop
    (`dispatch`) across one or more attempts via `Engine`, requiring
    `Deps.DB`.
  - `admission-controller` and `release-manager` needed no new route:
    their one existing endpoint is already the pipeline's entry point for
    their stage, so persistence + outbox emission wraps it in place.
- `deploy/kustomize/base/postgres-cluster.yaml` / Helm
  `templates/postgres-cluster.yaml` (CloudNativePG `Cluster`, gated by
  `postgres.cloudNativePG.enabled`).
- New env vars: `DATABASE_URL` (all five components, sourced from the
  CloudNativePG-generated Secret's `uri` key),
  `AI_SDLC_ENGINE` (worker only, default `stub`),
  `AI_SDLC_RELAY_ROUTES` (relay only, JSON object overriding individual
  `outbox.DefaultRoutes` entries), `AI_SDLC_ORCHESTRATOR_MAX_CONCURRENT`,
  `AI_SDLC_WORKER_LEASE_SECONDS`, `AI_SDLC_BASE_REF` (orchestrator;
  `baseRef` has no upstream source yet — defaults to `main`).
- `triggers.acceptanceCriteria` added to
  `contracts/events/v1/issue-admission-decided.schema.json` (additive,
  optional): the orchestrator needs it to build a work item without a
  second round-trip to re-fetch the issue, and ADR-0004's original schema
  didn't carry it.

### Changed
- Each component's HTTP handler persists state and appends to the outbox
  around the existing pure `Evaluate`/`Reconcile` call, instead of only
  returning the evaluation result. **The pure functions themselves are
  unchanged** — this ADR adds persistence and transport around them, it
  does not touch `admission.Evaluate`, `planning.Reconcile`,
  `cycle.Evaluate`, or `gates.Evaluate`.
- `deploy/kustomize/base/deployment.yaml` / Helm's deployment template
  gain a 5th container (`relay`) in the same pod, and a `DATABASE_URL` env
  var (from the CloudNativePG Secret) on all five.
- `NetworkPolicy` gains an egress rule from all five components to
  Postgres (port 5432). **No egress rule was needed between
  components**: `relay` reaches the other four over `localhost`
  (`internal/outbox.DefaultRoutes`), because all five are containers in
  one pod (ADR-0001's co-location decision) — that traffic never crosses
  a `NetworkPolicy` boundary.

### Explicitly still out of scope
- The real `scc`/`sc-agent-cli` subprocess `Engine` implementation (needs
  credentials not available here — tracked as follow-up once a target
  repo and model provider are chosen for the `homedir` pilot).
- GitHub webhook → `admission-controller` ingress wiring (the Route exists
  from ADR-0004; translating a GitHub webhook payload into
  `spec/normalized-event.schema.json` for this control plane is separate,
  reviewable follow-up, mirroring the GHA intake stage's existing logic).
- Multi-replica `relay` leader election beyond `SKIP LOCKED` (sufficient
  for a single adopter's throughput; revisit if `relay` is ever scaled
  past a handful of replicas).

## Rollback

Revert this ADR's commit. `runtime/k8s` returns to ADR-0004's
buildable-but-unwired scaffold; no component outside `runtime/k8s` is
touched.
