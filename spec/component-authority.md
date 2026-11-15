# Component authority, version 1

Issues are the only external work input. An issue author, comment, label text,
prompt, model response or generated file is data, never a component identity.
The reviewed installation supplies the [policy schema](component-authority.schema.json).
The [frozen example](fixtures/component-authority.v1.json) contains synthetic
public keys; it is not an installation identity or an activation configuration.

## Capability boundary

Each grant binds one producer, consumer, operation and event type. Repository
and environment are exact, case-sensitive scopes; there are no wildcards.
Unknown or missing grants deny access. Keys cannot be shared across roles.
Policy parsing rejects unknown fields, duplicate keys, trailing JSON values,
missing top-level fields and an implicit or null freeze value.

| Producer | Consumer | Operation / event | Minimum autonomy |
| --- | --- | --- | --- |
| intake | admission-controller | admit-issue / IssueObserved | shadow |
| admission-controller | orchestrator | plan-admission / IssueAdmissionDecided | shadow |
| orchestrator | worker | execute-work / WorkItemReady | auto-pr |
| worker | orchestrator | record-attempt / ImplementationAttemptDecided | auto-pr |
| worker | verifier | verify-artifact / CandidateProduced | auto-pr |
| verifier | release-manager | publish-candidate / CandidateVerified | auto-pr |
| release-manager | orchestrator | record-release / ReleaseGateDecided | auto-pr |
| release-manager | deployer | deploy-release / ReleaseReady | auto-deploy |
| deployer | release-manager | record-deployment / DeploymentVerified | auto-deploy |

Policies can raise these floors, never lower them. `shadow` can record internal
observations and admission decisions, but cannot execute changes or mutate the
target repository. `suggest` does not grant implementation authority. A freeze
denies every operational transition, including previously signed events.
Read-only health and evidence inspection use separate, non-transition APIs.
The installer and maintenance callers have no operational grants. Maintenance
may configure infrastructure; it cannot enqueue work or approve evidence.

A grant does not satisfy later stage-specific gates. In particular,
`publish-candidate` permits a verified proposal to reach the release manager;
it does not authorize a merge. Merge requires the trust-ladder level, risk
decision, independently collected checks for the exact head and applicable
review requirements. Deployment also requires a verified rollback path.

## Two signatures, separate lifetimes

The producer signs an Ed25519 **receipt** over its identity, durable event ID,
audience, operation, repository, environment, policy version and SHA-256 payload
digest. The operation selects exactly one event type from the policy. The
receiver separately validates that event's payload contract and scope. Store
the exact payload bytes (`bytea`); JSONB reserialization changes the signed
digest. Receipts do not expire just because delivery was interrupted. The
current policy must still authorize the operation and trust the producer key.

The producer or relay signs a fresh **transport authorization**, binding the
entire receipt, exact method/path, audience, scopes, current policy revision,
timestamp and random 192-bit nonce. It expires after 300 seconds, with at most
30 seconds of future clock skew. Separate signing domain prefixes prevent
using one signature type as the other. A relay can carry a fact but cannot
manufacture a producer's signature. Key rotation can retain an old producer
public key while outstanding facts drain, and revoke it to reject those facts.

`VerifyDelivery` verifies both layers. Successful signature verification is
not replay protection: the receiver must atomically claim the transport nonce
in a durable inbox, deduplicate the producer/event ID and commit state plus
outgoing events. Same ID and same digest returns the stored result; same ID
and different digest fails. Fresh transport signatures allow safe redelivery
of the original durable event. No effect may precede that transaction boundary.

After commit, the receiver signs a domain-separated Ed25519 acknowledgement
binding its identity, the policy revision, the receipt digest, transport nonce
and result digest. A relay must verify that acknowledgement before recording
delivery. An unsigned HTTP 200, a redirect or an acknowledgement for another
attempt is not evidence that the receiver committed the command.

## Trusted configuration and isolation

Policy comes from the reviewed deployment revision and is mounted read-only
outside generated workspaces. Per-component private identities are separate
Secrets. Issue content cannot select keys, policy paths, grants or Pod specs.
Do not mount control-plane credentials into generative or validation Jobs.

Signatures supplement database and cluster authorization. Separate database
roles must limit each component to its own state, append-only outbox production
and necessary evidence reads; a dedicated migrator owns schema changes. The
relay may update transport status, not another component's business state.
Executors need a separate namespace and enforced network policy. Sharing a
database superuser or granting Jobs access to control-plane Secrets defeats
these boundaries even when the HTTP signatures are correct.

## Implementation status and validation

`runtime/k8s/internal/authority` implements policy validation and cryptographic
receipt/transport/acknowledgement verification. `internal/delivery` implements
a signed HTTP handler, transactional inbox/outbox, nonce consumption, stored
responses and a relay bounded to eight attempts. PostgreSQL-backed tests cover
concurrent delivery, restart, rollback, response loss and component privileges.

These remain hidden integration building blocks. Component entry points now
register only observation routes and require
separate mounted signing and database identities. The installer selects the
dedicated migrator, but the signed delivery handler/relay remain disabled.
The installed runtime remains in maintenance.
These libraries alone do not establish an authenticated installed runtime or a
functional issue-to-production cycle. See the
[runtime integration boundary](../runtime/k8s/README.md#signed-delivery-integration-boundary).
Fleet signal resolution remains separately tracked by [kill-switch.md](kill-switch.md).

Run `go test ./internal/authority` from `runtime/k8s`; run
`bash tools/test-contracts.sh` from the repository root to validate the schema
fixture. Run `go test -count=1 -p 1 ./...` with `AI_SDLC_TEST_DATABASE_URL` pointing to a
dedicated disposable PostgreSQL database for durable delivery and ownership
tests. Deployment integration must prove these same boundaries and fail-closed
startup with the installed component identities before active mode is enabled.
