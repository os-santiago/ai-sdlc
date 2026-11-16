# Build controller process

The build-controller process accepts only authenticated `build-merged` inbox
messages. It has no advance, resume, override or source-selection endpoint.
`AI_SDLC_BUILD=true` additionally requires verification, independent review,
publication and merge capabilities. Missing/false build capability prevents new
work; invalid bootstrap flags prevent startup. Default installation remains
inactive until its separate Helm/policy/identity integration is configured.

A dedicated PostgreSQL session holds one fixed advisory lock while reconciling.
A one-second heartbeat checks session lock ownership and cancels execution on
loss. Unlock uncertainty discards the physical connection rather than returning
a locked session to the pool. This is coordination, not an atomic Kubernetes
fence: an API request can remain in flight after the database session disappears.

The journal supplies the safety barrier. Each new lock holder first terminalizes
interrupted claims and contains original acknowledged workload UIDs. A durable
intent without a UID remains quarantined and blocks new work. A late original
acknowledgement records identity for containment only; it cannot renew a claim,
start publication, or free the physical slot. Once containment is durably proven,
the next poll may select new work under current policy. Completed builds also
pass through containment; their artifact evidence remains immutable.

Current policy is reloaded from canonical inner/outer documents and must exactly
match the installed profile snapshot. An old snapshot cannot override a freeze.
Containment is selected before loading these policies, so missing, corrupt or
frozen policy does not suppress archived cleanup. Database credentials, coherent
bootstrap flags and Kubernetes access remain necessary to start or contain work.

The component uses its projected service-account credential against
`https://kubernetes.default.svc:443`. It must run in the same Kubernetes 1.36+
Konflux build cluster whose original identities are in the slots. It must not be
installed in the separate 1.35 runtime while pointing at another cluster's slots.
Its image is published as a supply-chain component, outside the default runtime
image lock until the installer explicitly enables it.

Validation covers competing SQL sessions, real backend termination, and a
blocked HTTP POST whose response arrives after leadership loss. The new leader
must stop at the unknown-create barrier; the old response is retained only for
cleanup. HTTP tests traverse the common runtime boundary and signed inbox with
an actual PostgreSQL journal. These tests are isolated component validation,
not the autonomous Saferus issue-to-production demonstration.
