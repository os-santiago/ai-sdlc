# Durable executor lifecycle ownership

Issue #495 adds an internal controller prerequisite. It does not install a
controller, grant operational database or Kubernetes permissions, receive a
live execution request, invoke a model, or change a workflow stage.

A [verified preparation](prepared-execution-inputs.md) contains immutable source,
assignment, profile and A-Dev bytes. Its 60-second claim is suitable for source
acquisition, but not a complete executor run. `execution.OpenLifecycle` verifies
that claim and the current signed command, issue/admission observation and
reviewed inputs again. It compiles the exact workload with installation-owned
namespace and immutable image references and binds its digest, source bundle
digest/length and resource names to one durable lifecycle.

## One transfer, no renewal

One PostgreSQL transaction locks the preparation claim, checks actual database
time after the lock, rotates its token and creates the unique lifecycle row.
The new deadline is the earlier of the signed request expiry and database time
plus the reviewed profile's `maxSeconds` plus a fixed 120-second setup/collection
allowance. The maximum is 1,020 seconds. A unique request reservation prevents
renewal; transaction rollback restores the original claim on conflicts.

The old preparation token cannot read, stop or extend the new owner's claim.
Concurrent reservations may produce only one current owner. Lost reservation
COMMIT acknowledgement returns no owner and attempts bounded cleanup using only
the newly generated token. A losing reservation cannot stop a different winner.
The profile duration is derived by the execution boundary from verified bytes;
the lower-level store methods are persistence primitives, not public authority.

The reservation is revalidated after commit. Current reviewed configuration must
be read on each loader call; cached configuration that conceals revocation does
not satisfy this contract. The installation loader supplies policy/intake inputs
and placement, never values selected by issue prose or generated code.

## Ordered effects and evidence

`Lifecycle.RunEffect` accepts a trusted controller operation for one fixed kind:

| Kind | Required previous phase | Recorded result |
| --- | --- | --- |
| `config` | reserved | ConfigMap UID |
| `pod` | config ready | Pod UID |
| `source` | Pod ready | Same Pod UID, exact source bundle digest and byte count |
| `start` | source ready | Same Pod UID |
| `export` | start ready | Same Pod UID, candidate digest/length and worker exit code |

The database locks the request before the lifecycle and persists an exclusive
intent token before the operation can run. It retains each intent and result in
`execution_effects`. A phase with an unresolved intent cannot advance or repeat;
a local mutex is only an additional guard, not the cross-process exclusion.
A simultaneous call on the same object is rejected immediately without stopping
the operation already in progress; it is not queued behind a callback.
The execution boundary rechecks signed authority, issue freshness, current
configuration and the nonrenewable claim after the intent commit and before the
operation. The operation receives independent input copies and a context whose
deadline is at most the lifecycle deadline. Result pointers are copied before
further configuration I/O.

The result transaction checks the exact effect token, claim token, lifecycle
binding and expected phase. Source/start/export results must use the acknowledged
Pod UID. A final database-time check after all updates catches expiration during
an effect-row lock wait and rolls back the entire acknowledgement. Current
execution proof is checked again after the operation and after the result commit.

Any callback error, panic, invalid result, cancellation, lost commit
acknowledgement or changed proof stops that in-process owner and attempts to stop
its durable parent with a bounded independent context. The library never retries
an external effect. A committed result remains available when its COMMIT ACK is
lost, but the parent is still stopped conservatively.

## Limits and unfinished integration

PostgreSQL ownership is not an atomic transaction with Kubernetes. A process can
crash after intent commit or after the remote effect but before acknowledgement.
Closing a connection, canceling a context or stopping the parent row does not
prove that a remote Pod/process terminated. A trusted operation must honor its
context and use the fixed bounded Kubernetes helpers; this generic internal
callback is not a sandbox for arbitrary functions.

There is deliberately no restart/adoption constructor. Durable records preserve
resource names, confirmed UIDs and uncertain intents for a future reconciler,
but this change does not implement orphan cleanup or guarantee it. If cleanup
cannot reach PostgreSQL, the existing intent and nonrenewable deadline remain;
no successful cleanup is fabricated. A future controller must connect these
records to fixed create/observe/delete operations, use UID preconditions for
cleanup, and revalidate authority at every provider/publication boundary.

An export record is untrusted collected evidence. A nonzero exit can be recorded
for diagnosis. Even a zero exit and a valid digest do not accept a candidate,
complete a work item, approve a PR or authorize a release. No operational role
receives access to these hidden tables in `ProvisionRoles`; fixture grants are
explicit and separate. Migration applies empty tables without enabling execution.

## Validation

Real PostgreSQL tests exercise concurrent reservation, stale/foreign tokens,
ordered phases, separate in-process objects contending on one durable intent,
actual lost server COMMIT acknowledgements for reservation/intent/result,
authority/placement/generation changes after intent, cancellation, callback
panic, result aliasing and immutable column permissions. Actual row-lock waits
cover request, lifecycle and effect rows; the effect-lock expiry regression
failed before the final database-time check and passes afterward. No live issue
state, Kubernetes workload or provider is used by these lifecycle tests.
