# Signed execution requests: controller prerequisite

The hidden `internal/execution` package accepts an execution command only through
the transactional signed inbox. The installed worker remains observation-only:
this change registers no HTTP route, grants no operational database privilege
and creates no workload. It supplies the durable authority boundary needed by a
future reconciler; isolated fixtures explicitly grant their own restricted role.

## Command and receipt

The versioned command binds one work item and attempt, repository, issue number
and generation, admission identity, immutable source SHA, reviewed profile digest,
authority revision and expiry. It contains no executable text, paths, credentials,
URLs or mount definitions. Its ID is a digest of its canonical typed content;
the separately signed receipt binds the exact original payload bytes.

The current reviewed policy must authorize the orchestrator's `execute-work`
operation to the worker, with at least `auto-pr` autonomy and no freeze. The
command must refer to that exact policy revision and expire within one hour.
Transport signature, nonce consumption and inbox deduplication use the existing
delivery boundary. The effect reloads current policy and validates business
shape before inserting the immutable request in the same transaction. A failed
effect rolls back the nonce and inbox record too.

PostgreSQL constraints allow only one request for a work-item/attempt identity
and one active queued/claimed request per work item. Reusing an identity with
changed content fails. A signed duplicate with a fresh transport nonce returns
the previously committed result. An administrator credential cannot impersonate
the worker database identity through these APIs.

## Claim and uncertainty

Claim reloads the stored receipt and exact command, checks immutable columns,
revalidates current authority and the admission proof below, then obtains an exclusive row lock. Actual
database time is checked after waiting for the lock. Only a queued, unexpired
request can become claimed. A fresh random token and a lease of at most 60
seconds are committed before any capability is returned. Concurrent losers
cannot receive or stop the winner's token. Existing claims are never reclaimed.

After the commit acknowledgement, the caller reloads policy and intake configuration,
rechecks admission proof, and checks lifetime and cancellation. The final policy
reload also detects changes during the bounded proof query. Revocation during the row-lock wait stops only its
own claim and returns no capability. A lost commit acknowledgement also returns
no capability and attempts bounded cleanup through an independent context.
Cleanup requires the caller's exact token. If the database remains unavailable,
the row may remain claimed; it is never reopened, and a future reconciler must
inspect and stop it. No retry authorizes an external effect from that uncertain
outcome. A lost inbox commit acknowledgement is handled by durable deduplication.

There are no external effects inside the inbox or claim transaction. A claim is
not evidence of a running Pod, generated artifact, successful validation or
release. The policy can change again after a check; each later external-effect
boundary must revalidate current authority and the live claim.

## Current admission and ownership proof

A bounded SQL snapshot joins the work item, owning repository and plan, current
intake/admission/planning heads, exact intake revision, stored admission, and
polling evidence. All heads must identify the command's generation. The three
stored snapshots must match canonically and validate against the current intake
configuration, authority revision and immutable source SHA. The admission must
be accepted and low risk; its identity, policy version, reasons and required
gates must match the stored decision. Required gates remain obligations, not
proof of successful validation.

The plan correlation must identify that exact intake event. The work item must
be ready, refer to exactly that issue, preserve the extracted acceptance criteria,
and have an explicit empty dependency list. This initial boundary rejects work
that needs dependency resolution instead of assuming its dependencies succeeded.

Freshness requires both a successful overall scan and an individual observation
within twice the configured poll interval, capped at five minutes. A scan of
other issues does not refresh the target. Intake records the beginning of its
actual source reads and preserves that timestamp through persistence, including
unchanged snapshots. Observation writes require the intake identity and a still
valid lease after waiting for the issue-head lock. They bind the generation,
configuration, content and authority revision. Missing observations after an
upgrade fail closed until intake actually rereads the issue.

The worker cannot update those observations. Operational role provisioning still
grants no worker access to this proof or the hidden execution queue; isolated
fixtures grant only the columns needed for the tests. Changes discovered after
claim commit stop only the caller's token and return no capability. As with any
observation, an issue can change after it was read; every later external-effect
boundary must revalidate. This is bounded freshness, not a distributed lock on
GitHub or proof that the source remains unchanged forever.

## Remaining integration gates

Before operational activation, the reconciler must acquire the pinned source,
verify the reviewed profile and A-Dev bundle, prepare the bounded workspace,
provision and bind an isolated Pod, allocate the durable model budget, and
reconcile expiry, policy changes and uncertain external operations.

The model dispatcher and Pod-bound transport remain separate prerequisites.
Candidate extraction requires independently observed writer termination and
verification; publishing, merge, provenance, release and deployment gates remain
mandatory afterward. No supervising session may replace those transitions.
Live requests must still enter through issues.

## Evidence

Tests use a disposable PostgreSQL database and distinct component logins. They
exercise forged delivery, invalid commands, exact replay, concurrent claims,
immutable-column privileges, cross-role denial, expiration after a real row-lock
wait, revocation during that wait and token-scoped termination. Wire fault
injection drops actual server COMMIT acknowledgements, then permits reconnection
to verify inbox deduplication and stopped uncertain claims. Reapplying the normal
role provisioner removes the fixture-only grants. These tests do not advance a
live workflow or demonstrate an autonomous issue-to-production cycle.

Additional PostgreSQL regressions reject mismatched ownership, generation,
admission, criteria, gates, configuration, source and freshness; change a head
while a claim waits on a real row lock; and demonstrate that a successful scan
omitting the target leaves its observation stale. Dedupe refreshes only a target
actually reread, without another business event. An expired intake lease cannot
refresh an observation after a confirmed row-lock wait. Worker writes remain
denied even with a fixture-only accidental column grant, and normal privilege
reconciliation removes every fixture-only proof read grant.
