# Authenticated deployment completion

A successful Kubernetes deployment is evidence for a separate terminal release
operation. It does not itself create a GitHub Release or close an issue.
`record-deployment` is the existing signed outerloop grant from deployer to
release-manager. This increment defines its bounded command and transactional
handoff; it does not wire a GitHub writer or enable delivery.

The deployer selects only an authenticated `deployed` record with fresh issue
eligibility and unchanged current policies, target, provenance trust and build
profile. Rollback and quarantine never qualify. The command commits to the
original deployment ID, complete record digest and current configuration. Its
separate five-minute recording window is bounded by the original approved target
and provenance expiry; it grants no new deployment/rollback authority and never
extends the original deployment plan.

The historical reader reopens the original claim, authenticated deployment
request, conditional intent, one-use send, acknowledged receipt, health and all
upstream promotion/provenance/build/merge/publication records. Release-manager
receives only SELECT access to those deployment tables and cannot alter deployer
state. The public command contains no claim token. Constructing a record or
verifying its structure does not establish origin; only the role-checked durable
reader does so.

A completion handoff and signed outbox entry commit in the same PostgreSQL
transaction. The handoff is immutable and unique per deployment. Fresh policy,
issue and exact historical digests are checked before and after writes. A failed
outbox insert rolls back the handoff. A second dispatcher sees the completed
handoff and cannot emit a replacement command with a renewed window.

## Evidence and limits

Real PostgreSQL tests cover role separation, complete historical validation,
signed handoff, duplicate exclusion, tampered evidence, withdrawn issue, atomic
outbox failure and rollback rejection. Kubernetes observations in these tests are
synthetic; this is not an actual production deployment or an autonomous E2E.

The terminal journal, exact-SHA tag/release writer, relay/inbox wiring, issue
closure and orchestrator completion remain separate required work. Intake must
not be enabled as a complete E2E demonstration until that path is installed and
validated. A publication or merge is never evidence of production completion.
