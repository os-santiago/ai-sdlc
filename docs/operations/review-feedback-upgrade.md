# Install retained review feedback

`runtime-upgrade review-feedback` is the closed schema-35-to-36 administrative
transition after a completed [browser installation](browser-verifier.md).
It preserves expired, cleaned failed reviews and installs retention for future
authenticated rejections. It does not repair candidates or recover discarded text.

Use the exact clean checkout of the successful immutable main container
publication whose catalog adds only `0036_review_feedback.sql`. The target chart
must be byte-identical to the published source chart. Supply the separately
reviewed provenance document changing only `verifierImage` to that publication's
verifier digest; all keys, scope, registry CA and approval dates remain unchanged.
The existing release target and approval window must still be valid.
Catalog equality alone is insufficient: an older schema-36 publication may lack
the new fence. The selected publication commit must equal the local HEAD, with no
tracked changes or untracked files, including when resuming the original intent.

```bash
./install.sh runtime-upgrade review-feedback \
  --cluster <original-cluster> --provider podman \
  --target-run <successful-main-publication-run> \
  --target-revision <full-main-commit> \
  --provenance-trust <reviewed-private-document.json>

./install.sh runtime-upgrade review-feedback-status \
  --cluster <original-cluster> --provider podman
```

The new operation has its own journal and at most 30 minutes, bounded further by
the original approvals. It never changes its parent's nonce, deadline or backup.
Repeating the command observes the same recorded operation; it cannot select a
different publication, refresh an expired intent, replay an unknown migration or
renew workflow authority.

Before pausing producers, the installer requires the original Helm resources,
chart, database Pod and browser namespace identities. It rejects residual browser
Pods, Jobs or CronJobs, active work, incomplete cleanup, unknown effects and any
downstream delivery history. It anchors database schema, credentials, roles,
memberships, settings and migration timestamps to the original proven schema-35
snapshot. Legitimate expired review history may have accumulated since that
snapshot; the first write fence captures it, permitting only already-bound intake
polling changes between the preliminary observation and that fence.
The new fence also retains `blocked` work only when its complete candidate and
expired, cleaned failed-review chain exists. A blocked item without that evidence
is rejected. The earlier reviewer/browser predicates remain unchanged.

The installer pauses all ten Deployments under the database write fence, exports
and restores a private backup, then invokes the published migrator once. The new
feedback table must be empty and match the independently checked PostgreSQL
descriptor. Every original row, role and privilege remains intact, including the
five empty browser tables. No backup is restored over the operational database.

The image change is limited to migrator and verifier. The latter is shared by
`ai-sdlc-verifier` and `ai-sdlc-provenance-verifier`; other consumers retain their
original images. The installation therefore explicitly records a mixed
publication. Worker images, budgets, browser profile, RuntimeClass, publisher and
delivery capabilities remain unchanged. Staging retains the original resource
UIDs; warming starts consumers frozen, arming opens policy, and the final phase
changes only intake replicas.

An uncertain mutation or expired window leaves its journal and containment
requirements intact. This command offers no automatic rollback or recovery to old
consumers after migration. A status report is installation evidence only, never
proof of autonomous issue-to-production delivery.

## Validate before applying

Python tests render the actual chart and exercise original authority, residual
browser resources, expiry and unknown migration rejection. PostgreSQL tests cover
the distinct fence, retained failed review history, writer exclusion and protocol
identity. The disposable composition adds real backup/restoration, migration,
credential authentication, metadata verification and negative ACL/RLS/history
controls:

```bash
python tools/review_feedback_upgrade_fixture.py \
  --provider podman \
  --source-run <successful-schema35-main-run> \
  --source-revision <full-schema35-main-commit> \
  --target-image quay.io/os-santiago/ai-sdlc@sha256:<local-reviewed-fixture-digest> \
  --output <new-private-output-directory>
```

The planner-produced `blocked` positive case uses an independent PostgreSQL
instance because component roles are cluster-wide. Its reproducible command,
also run by local-validation CI, is:

```bash
python tools/validate_feedback_planner.py --provider podman \
  --output <new-private-planner-output-directory> --timeout 300
```

It invokes `BlockSupersededIssue` through the orchestrator's real database login,
then proves the new fence preserves the result in both schemas while the earlier
protocols still reject it. The general suite skips this separately gated fixture
instead of adopting another test database's existing roles.

`tools/validate_feedback_browser_guard.py --help` provides the isolated kind
negative controls, also run by installer CI. It requires an explicit cluster UID,
creates `ai-sdlc-browser` only when absent, and checks empty versus residual
Pods/Jobs/CronJobs and namespace replacement. Pods stay scheduling-gated; Jobs and
CronJobs stay suspended. Namespace cleanup uses the original UID precondition.
This is a residual-work guard test, not a complete upgrade or readiness proof.

For the migration composition above, the target is a local test image, not publication
approval. The fixture creates and removes its own PostgreSQL containers; it never
targets an installed runtime. Its success does not prove the complete Kubernetes
transition. Verify the ten original Deployments and their readiness after any
authorized application, and report that result separately from fixture evidence.
