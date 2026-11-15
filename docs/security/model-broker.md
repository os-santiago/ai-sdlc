# Bounded provider dispatch: hidden controller prerequisite

Issue #471 composes the [model ledger](model-call-reservations.md), immutable
executor profile and mailbox envelope into a bounded provider dispatch library.
It is not installed or exposed as a service. It adds no public route, database
grant, provider Secret, deployment or live workflow transition.

## Trust and invocation

A future trusted controller must establish current signed execution authority
before calling `Dispatcher.Deliver`. Ledger membership is not that authority.
The library reads immutable allocation bindings, verifies the exact profile
digest and validates the request against its attempt, Pod UID, model and token
limit. It reserves the exact request bytes and invokes only after a newly
committed claim. Concurrent deliveries cannot each obtain that claim.

Provider configuration is reviewed out of band. Requests cannot change the
endpoint, bearer credential, proxy, redirect handling or timeout. HTTPS requires
certificate verification and TLS 1.2 or later. Explicit local development HTTP
allows only literal loopback IP addresses. The endpoint path is fixed to
`/v1/chat/completions`; URL credentials, queries and fragments are rejected.

The transport uses fresh HTTP/1 connections without keep-alive, HTTP/2, ambient
proxies, redirects, compression or a replayable request body. There is one HTTP
request attempt, with a 30-second client limit, three-second connection/TLS
limits, 25-second response-header limit and 8-KiB response-header bound. The
header limit accommodates the local adapter's existing 24-second buffered
completion bound; earlier cancellation still interrupts header waiting. The
dispatch deadline is also capped by the executor profile and by the remaining
call/mailbox lifetime minus a 15-second handoff margin. No failed HTTP exchange
is retried. This does not imply exactly-once execution inside the provider.

Responses must be status 200, JSON, valid UTF-8, within the reserved response
limit and contain one to eight assistant choices. The original response bytes
are retained for publication. Generated text and tool calls remain untrusted
data; this library does not execute or authorize them.

## Uncertainty and delivery

Reservation and claim errors may represent committed transactions with lost
acknowledgements. The deterministic call reference is retained before either
operation. On uncertainty, an independent, bounded cleanup context attempts to
stop that call and its parent budget; no provider invocation is granted. The
same cleanup applies to failed provider requests, completion and publication.
If PostgreSQL remains unavailable, cleanup cannot prove that it stopped the
parent. The outcome stays uncertain and expiry/reconciliation is required;
this library does not promise to revoke another already-started call.

Completion metadata is committed before publication. A trusted `Publisher`
capability must check the actual Pod UID, enforce exclusive publication of the
exact envelope through the mailbox helper and honor its five-second context.
After confirmed publication, the ledger records the exact response digest,
status and size. Repeated completed/acknowledged deliveries return metadata
only. They neither invoke the provider nor republish the response.

Publication failure stops the budget and preserves completed response evidence
as ambiguous. Mailbox publication is not proof that SCC consumed the response.
The hidden [Pod helper transport](pod-helper-transport.md) implements this
capability with a helper receipt and external Pod/container checks. A caller-owned
callback or successful socket write is not operational evidence of that boundary.
Provider bodies and credentials are absent from returned results and error strings.

## Validation and remaining integration

Tests use real isolated PostgreSQL and HTTP/TLS sockets. They exercise concurrent
delivery, exact request/response bytes, provider errors, redirects, oversized
headers/body, interrupted TCP bodies, invalid JSON/UTF-8, cancellation, failed
publication, mismatched bindings, proxy isolation and TLS verification. A wire
proxy drops actual PostgreSQL COMMIT acknowledgements after reservation and
claim, then permits reconnection; independent reads verify parent shutdown,
zero provider calls and denial of a sibling allocation's claim.

These are component fixtures with synthetic provider responses. They do not
demonstrate a Codex call or an issue-to-production cycle. Live integration still
requires reviewed controller authority, least-privilege database roles,
provider provisioning, reconciliation and
candidate/release/deployment gates. Call/byte limits are not monetary budgets.

## Current authority at external-effect boundaries (issue #501)

`Dispatcher` now requires a trusted `CurrentAuthority` capability. Omitting it
fails before reservation. Its `Authorize(ctx, scope)` method must revalidate
current signed execution ownership, the exact workload/Pod/repository/source/
profile binding and reviewed configuration, then return the nonrenewable
execution deadline. Ledger scope alone does not supply these facts. The broker
provides no default production authorizer and enables no operational route.

The broker checks this capability after validating immutable scope, again after
the committed call claim immediately before invoking the provider, and after
recording the response metadata immediately before publishing to the mailbox.
This closes the unchecked waits across database locks and provider processing.
Every check receives a context bounded to five seconds; implementations are
trusted and must honor that context. A check error, panic, cancelled context or
expired deadline is rejected. The full delivery retains the earliest deadline
observed, even if a later authorizer returns a later time.

Denial after a committed claim or completed response uses the existing
conservative stop path. Response hash, byte count and status remain available
in the ambiguous call record; no raw response is persisted or published after
that denial. No automatic provider or publication retry is introduced. A
previously handed-off duplicate still needs current authorization before
returning metadata, and does not invoke either external capability again.

The checks are not atomic revocation of an already in-flight remote operation.
A response published before a revocation becomes observable cannot be retracted
by a later database update. Handoff acknowledgement retains its existing
bounded evidence-recording behavior; it is not a new provider invocation.
A concrete lifecycle-backed authorizer and its installed configuration remain
required before operational integration.

PostgreSQL/HTTP tests revoke authority while a real reservation waits on a row
lock and while the provider handles its request. They verify zero invocation in
the former case, zero publication and retained response metadata in the latter,
and prevent a later authority check from extending the initial deadline. The
isolated kind and real-model developer fixtures explicitly use synthetic
authority; their success does not prove live signed execution authorization.
