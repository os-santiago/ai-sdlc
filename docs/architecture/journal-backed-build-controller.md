# Journal-backed closed build controller

`ExecuteQueuedBuild` accepts only an identifier of a stored signed build request.
It reads the original merged-source record and current issue/installation authority,
compiles the closed recipe, claims one unused physical slot, and performs the
installed preflight before a durable intent and one-shot Kubernetes create.

Every observation and transition reloads authority and issue freshness. The original
builder's termination is recorded before a publisher intent; the original builder
is observed again before publication. Both terminal observations and the exact
publication result must commit before a built record exists. That record still has
`releaseAuthorized: false`.

The transactional inbox commits its nonce, response and queued request together.
The selector excludes requests that cannot fit the complete remaining build budget,
changed policy/profile/intake revisions, superseded or ineligible issues, stopped
work, and stale intake observations. Selection is only an optimization; fresh
checks remain mandatory when reading, claiming and executing.

## Cleanup custody

A successful POST response is recorded as original cleanup identity even if freeze
or expiry happens immediately afterward. The receipt is bound to the existing
claim, physical slot, plan, effect token, request digest and resource name. It
cannot grant continued execution. A later fence stops the attempt.

Receipt recording and cleanup closure lock the same request row. The receipt checks
for an existing cleanup record in a new statement after acquiring the lock, so a
Read Committed snapshot taken before waiting cannot bypass closure. Cleanup rereads
its original resource set under that lock before committing. A receipt arriving
after closure is rejected; unknown creates remain quarantined and physical slots
are never returned to the pool.

## Evidence and pending installation

The real isolated composition fixture uses disposable PostgreSQL with explicitly
synthetic issue/publication/merge history. It then runs the production controller
against actual Kubernetes/Tekton build and publication workloads, reads the durable
built record, rejects resumed work and slot reuse, and removes fixture resources.
It is not an autonomous issue-to-production demonstration.

No component Deployment, public advancement route or installer activates this
controller yet. Operational original-UID cleanup/containment, signed dispatch from
the release manager, provenance consumption, promotion/deployment and independent
acceptance are required before activation. Freeze alone is not proof of quiescence:
the installer must observe stopped workloads before changing their isolation,
credentials, node, storage or trusted dependencies.
