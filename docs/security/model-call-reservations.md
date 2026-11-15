# Durable model-call reservations: hidden controller prerequisite

Issues #463 and #469 add a PostgreSQL ledger and response-handoff barrier for a
future trusted model controller.
It is not a provider, an authorization endpoint, a new runtime service, or a
completed autonomous flow. No current component calls it, and none of the eight
operational database roles can access its tables. The executor and
[offline mailbox](offline-model-mailbox.md) receive no database credentials.
The hidden [provider dispatcher](model-broker.md) composes this ledger with
bounded HTTP and a trusted publication capability; it has no live wiring.

## Immutable scope and finite limits

One `model_budgets` row belongs to an existing running work item and binds its
repository, source commit, policy digest and exact model identifier. Policy is
insert-only through this API. Recreating it cannot reset counters. Each
`model_allocations` row binds one attempt number to one Pod UID. Another Pod
cannot take over that attempt, and a new attempt consumes the same parent budget.
An allocation must fit inside its parent's limits and expiration.

| Scope | Calls | Request bytes reserved | Response bytes reserved | Lifetime ceiling |
| --- | ---: | ---: | ---: | ---: |
| Work item | 128 | 16 MiB | 32 MiB | 24 hours |
| Allocation | 32 | 4 MiB | 8 MiB | 1 hour |
| Individual call | 1 | 128 KiB | 256 KiB | 60 seconds |

These are implementation ceilings; reviewed policy should set lower limits as
appropriate. Every database operation has a ten-second context deadline. The
mailbox has its own earlier deadline, which a future controller must also honor.

Reservation validates the mailbox's strict top-level request schema and compares
its actual `model` field with policy. It hashes the exact request bytes and
charges their actual size. Nested messages and tools remain untrusted provider
input; this is not validation of tool authority or a complete provider schema.
No executor-provided byte count, destination, claimed budget or digest is trusted.

The reservation charges one call and the allocation's **maximum response size**
up front. Completion, failure, timeout and cancellation do not refund it. Parent
and allocation counters update in the same transaction. Different allocations
cannot spend the same remaining parent budget concurrently. There is at most one
pending call per allocation, with consecutive sequence numbers. An exact repeated
reservation returns existing metadata; changed bytes, identity or model stop the
parent budget. No API resumes a stopped budget.

## Dispatch and uncertain outcomes

```mermaid
stateDiagram-v2
    [*] --> reserved: reserve bytes and call
    reserved --> started: confirmed claim commit
    reserved --> stopped: cancellation or expiry recovery
    started --> completed: bounded response before expiry
    started --> ambiguous: uncertainty or expiry
    completed --> handed_off: exact mailbox publication acknowledged
    completed --> ambiguous: publication lost, abandoned or expired
    handed_off --> [*]
    stopped --> [*]
    ambiguous --> [*]
```

All mutations lock parent budget, allocation and call in that order. Expiration
uses PostgreSQL `clock_timestamp()` after the shared locks, rather than the
transaction's start time. A successful `ClaimModelCall` returns `Invoke=true`
only to the process whose new `reserved -> started` transaction committed.
Existing `started` or terminal state never authorizes another dispatch. Errors,
including an uncertain commit acknowledgement, return no invocation grant.

A future controller must disable implicit provider retries, verify the exact
reserved body immediately before dispatch, honor both call and mailbox deadlines,
and reconcile any uncertain outcome. Reading `started` after a restart does not
prove whether the provider received the request. Recovery marks it ambiguous and
stops the parent budget; it never tries again automatically. Expiry recovery
examines at most 100 calls per invocation, serializing with completion. Every
reservation and claim also checks for expired pending calls across the parent
budget under its lock. A different attempt cannot dispatch while an expired call
awaits the next recovery sweep.

Completion records only response byte count, SHA-256 and supported HTTP status.
It cannot reopen stopped state. Exact repeated completion is idempotent; conflicting
completion metadata stops the parent budget. If a response is lost after metadata
was committed, **the response body cannot be recovered from this ledger**. Neither
reconstructing it nor making another provider call is an automatic replay policy.

Completion alone does not permit a next call. A completed response with no
`response_handed_off_at` blocks both new reservations and claims of previously
reserved calls across the entire work-item budget. This barrier is checked under
the parent lock after all row locks and the database clock refresh. Calls already
started before the completion cannot be retroactively revoked by this barrier.

Only a trusted dispatcher may call `AcknowledgeModelResponse`, after the
UID-bound mailbox helper confirms exclusive publication of the exact response.
The acknowledgement binds allocation, Pod UID, sequence, request hash, response
hash, status and byte count. Its timestamp comes from PostgreSQL and never
changes on exact replay. An uncertain acknowledgement commit returns no grant;
a fresh connection may reconcile the same metadata without invoking a provider.
`handed_off` in the diagram is a logical milestone: the row remains `completed`
with the acknowledgement timestamp populated.

Abandoning or expiring an unacknowledged completed response marks the call
ambiguous and stops its parent, retaining the known response hash/status/size.
The bounded recovery sweep includes these responses, and an expired sibling
blocks spending before any sweep runs. Late acknowledgement never reopens a
stopped budget. The migration does not invent acknowledgements for older
completed rows; they require reconciliation or fail-closed expiry.

**Mailbox handoff is not proof that SCC consumed a response.** A response already
published but lost before application receipt can still lead SCC to attempt a
new sequence. End-to-end receipt/reconciliation or an explicit retry policy is
still required. This ledger does not promise exactly-once application delivery.

## Authority and remaining boundaries

The ledger is a persistence primitive, not permission to execute a work item.
Provisioning and dispatch still require a reviewed controller identity, current
signed execution authority, immutable Pod/image/template checks, fixed provider
configuration, credential separation, output validation and a cancellation
protocol. Merely having a running work-item row is not sufficient authorization.
There is intentionally no HTTP route, new role grant, provider client or live
Job integration in this change. Migrations are applied by the dedicated migrator;
new component images require migration 0005 before becoming ready.

Calls and bytes are not tokens or money. Provider billing, account-wide and
installation-wide budgets, concurrency across work items, retention/archival and
verified usage reporting remain separate work. Unknown cost remains unknown.
Database owners remain trusted: direct privileged SQL can change policy or
counters. Operational roles have neither table nor column grants to this ledger.

## Real validation

From `runtime/k8s`, set `AI_SDLC_TEST_DATABASE_URL` to a disposable PostgreSQL
instance and run:

```sh
go test -count=1 ./internal/store -run TestModel -v
```

The tests create fixture work items, never advance live issues, and cover:

- Concurrent identical reservations, shared budgets across attempts, and a single
  winning claim among concurrent controllers.
- A separate connection observing `started`, exact replay, changed request/model,
  replaced Pod, skipped sequence, pending-call and byte-limit rejection.
- Expiration while blocked on a row lock, completion/recovery races, response
  overflow, cancellation and no refund after an uncertain outcome.
- A real PostgreSQL wire proxy that drops the **COMMIT acknowledgement after the
  server committed**; no invocation grant escapes, and a fresh connection cannot
  claim the same call again. This test requires the isolated fixture's
  `sslmode=disable`; no production connection is intercepted.
- Actual access denial for all eight current runtime roles after role provisioning.
- Cross-allocation barriers for new reservations and prior claims; exact
  acknowledgement races, conflicting response bindings and immutable timestamps.
- Real lost COMMIT acknowledgements after both completion and mailbox handoff;
  metadata reconciliation never returns provider invocation authority.
- Handoff expiry while blocked on the budget, allocation or call row, observed
  through PostgreSQL lock-wait activity; response evidence survives abandonment.

This evidence validates the isolated persistence component. It does not prove a
provider call, SCC-generated improvement, release provenance, deployment or
issue-to-production E2E delivery.
