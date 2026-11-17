# Signed deployment handoff and bounded controller

Release-manager dispatches a completed authenticated promotion through one
immutable handoff and the signed transactional outbox. Relay transport is
separately authenticated by the deployer inbox. Duplicate transport cannot queue
a second attempt. Selection and dispatch recheck current issue revision,
generation, eligibility, work status, trust, profile, policies and target. Neither
a request parameter nor model output can select an artifact or destination.

The controller receives a database identity, trusted mounted configuration and
a platform-owned console adapter. It selects queued work, claims it exclusively,
observes baseline identity and health, and creates the durable intent before the
image-only update. The adapter's gate consumes the one-use send fence. The
controller records the exact acknowledged response before rollout and health
validation. Durable completion is the only success result.

If readiness, health or final current-authority checks fail after a recorded ACK,
the controller attempts the original bounded baseline restoration. It requires
unchanged destination custody and the still-owned candidate generation. Every
adapter operation inherits the original claim deadline, including rollback and
health. A slow RPC cannot inherit the longer target approval as extra time.
Cancellation/expiry is quarantine, not new background deployment authority.

An ambiguous API response cannot trigger rollback from image equality. Lost
journal ACKs are resolved only by reading the exact immutable receipt or complete
health record already committed. There is no replay of an effect to recover its
acknowledgement. Terminal results and quarantines prevent selection of that
attempt again.

## Mounted authority and containment

`releaseflow.Mounts` reopens the independently mounted canonical policies,
reviewed build configuration, public provenance trust, approved target and the
component's own signing identity. It grants only verifier, release-manager or
deployer roles as appropriate. Stale policy snapshots and mismatched installed
verifier images reject new work.

The target loader is separate so that withdrawing advance policy does not erase
already granted containment authority. The journal compares this freshly loaded
target with the original immutable target and checks its expiry. Missing,
malformed, changed or expired targets deny containment. Original restoration
permission is also stopped by withdrawing database or Kubernetes permissions.

## Validation and remaining integration

Real disposable PostgreSQL tests exercise signed dispatch, independently signed
relay transport, inbox deduplication, real COMMIT acknowledgement loss, concurrent
claims, observed SQL lock withdrawal, successful synthetic controller completion,
rollback, quarantine and mounted-target failures. A real local TLS API test delays
a rollback response beyond the caller deadline and requires an uncertain result.
The separate kind fixture covers actual conditional API update and baseline
restoration.

The controller-contract observations are synthetic; they do not establish a
successful candidate deployment or autonomous issue-to-release run. This
increment supplies runtime composition and its service boundary. The deployer
executable/image, chart, approved destination preparation, restart containment,
terminal GitHub release and activation remain required before enabling intake.
