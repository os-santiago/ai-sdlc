# Reviewed image-lock upgrades

The installer consumes immutable image digests from Git. A newer source checkout
alone does not change the installed images. Prepare the next lock from an exact,
successful `containers` publication on `main`:

```bash
python3 tools/install_images.py --run RUN_ID --revision FULL_COMMIT_SHA --output /tmp/reviewed-ai-sdlc-images
```

Use Python 3 on Linux and an authenticated `gh` CLI with read access to the
repository's Actions artifacts. Both identifiers are explicit: the tool never
selects an implicit latest build or accepts PR/build-branch artifacts. The output
directory must be new. The command reads GitHub and writes local files only.

The resolver requires all nine component identity artifacts from the same
successful push run, exact source revision, repository, workflow and platform.
It verifies each downloaded ZIP against GitHub's reported SHA-256 and size,
accepts one bounded JSON entry, rejects duplicate fields and checks all identity
fields. GitHub responses have a 1 MiB file limit, 30-second request limit and
180-second overall work limit. Missing, duplicate, expired or mismatched
artifacts fail before an installer lock is written.

`images.lock.json` contains the six installed services and migrator, in the
installer's existing schema. `evidence.json` also records the model-mailbox image and the optional verifier
image. Verification mode requires the latter from the exact same publication.
Review the generated lock and evidence in a focused PR, retaining the workflow
URL and source revision. Commit the reviewed lock to
`runtime/k8s/deploy/local/images.lock.json` and its public publication record.

## Plan a transition before changing an installation

The single entrypoint can compare two explicit, successful `containers` push
publications on `main` without accessing the cluster:

```bash
./install.sh upgrade-plan \
  --source-run SOURCE_RUN_ID --source-revision FULL_SOURCE_COMMIT_SHA \
  --target-run TARGET_RUN_ID --target-revision FULL_TARGET_COMMIT_SHA
```

Run it from a checkout containing both exact commits. Missing Git objects fail;
the planner does not fetch or change the checkout. Git, Python 3 on Linux and
an authenticated `gh` CLI are required. It resolves all twelve image identities
through the existing publication verifier, then reads migration SQL directly
from immutable Git objects with replacement refs and legacy grafts disabled. Modified working
files cannot alter the plan. Source must be an ancestor of target, or the same
commit. Removed or edited migrations, or new files inserted before the original
ordered prefix, reject the plan. Symlinks, nested paths, oversized SQL and
noncanonical migration names also reject.

The JSON report lists both revisions, publication URLs, image identities,
changed components, and pending migration filenames and SHA-256 values.
`pendingMigrations` means absent from the **source code catalog**, not observed
absent from a database. Identical catalogs produce an empty list; that does not
prove an installation is current. The planner never runs migrations, reads
cluster credentials, scales controllers, changes policies or advances delivery work.

This is the first preflight gate for [runtime upgrades (#441)](https://github.com/os-santiago/ai-sdlc/issues/441),
not an upgrade command. `upgradeAuthorized` and `rollbackAuthorized` remain
false. `unverifiedGates` records the remaining installation identity, database,
configuration/consumer compatibility, draining/containment, restored backup,
artifact trust and rollout/recovery checks. Filename/hash continuity cannot
prove that an appended SQL file is nondestructive or reversible. Existing
`schema_migrations` rows attest filenames only, not hashes of historical SQL or
current schema integrity. Publication identity is not a provenance attestation.

An active installation with history must retain that history and use a validated
upgrade procedure. Do not substitute the empty-bootstrap preparation or
initial activation commands for that procedure.

## Apply and verify the desired state

Keep the installation's existing repository, environment and profile. For the
Homedir pilot, first commit the reviewed lock at the path already named by its
configuration. Then the scope remains shadow intake in its dedicated kind cluster:

```bash
./install.sh plan --config examples/homedir.local-install.json
./install.sh local --config examples/homedir.local-install.json
python3 tools/verify_shadow_intake.py --cluster ai-sdlc-homedir --provider podman --repository os-santiago/homedir --issue 1576 --expected needs-human --timeout 20
```

For an already authorized candidate installation, preserve its candidate profile:

```bash
./install.sh plan --config examples/homedir-candidate.local-install.json
./install.sh local --config examples/homedir-candidate.local-install.json --github-auth gh-cli --codex-auth-file /private/path/auth.json
```

The current lock publishes source `aae96016600e634645f0cbb7fdda446d95279200`
from [containers run 43743992582](https://github.com/os-santiago/ai-sdlc/actions/runs/43743992582).
It includes bounded model handoff diagnostics and the inactive independent
verification foundation. Candidate mode still grants no verifier dispatch,
forge publication, merge, release or promotion capability.

Do not combine that configuration with a different `--images` override: the
installer deliberately rejects conflicting declarations. To test a separate lock,
review a separate configuration file that explicitly names it.

Before an existing database upgrade, retain a private recovery copy and verify
restoration against a disposable database. Do not commit database dumps or
Secrets. Check the migrated version list, rollout readiness and retained signed
intake evidence. New per-issue observations must be written by intake itself;
never manufacture them through SQL. For a disposable restore database, wait
for TCP readiness (`pg_isready -h 127.0.0.1 -U USER -d DATABASE`) before
restoring globals and data. The PostgreSQL image temporarily serves a local
socket while initializing; socket readiness can precede database creation. Reapply the same installer inputs to verify
convergence and preserve both attempted runs' logs.

Migration scripts currently track applied filenames; they do not attest hashes
of previously executed SQL. Downgrading images does not reverse a database
migration. Recovery needs a reviewed compatible image/schema combination and
a tested backup, not an assumed automatic Helm rollback.

## Observe migration tracking without changing history

The migrator exposes two administrative observation operations:

- `describe-migrations` emits the ordered filename/SHA-256 catalog compiled into
  that exact binary. It has no database dependency.
- `inspect-upgrade-migrations` accepts one private JSON document on stdin with
  exactly `databaseURL`, `database`, and `sourceCatalog`. The source catalog must
  be the unchanged prefix of the binary's embedded target catalog. Independently
  bind it to the reviewed original source/publication before observation; a
  caller-supplied catalog alone is not source evidence. Older binaries without
  the descriptor require catalog extraction from their exact reviewed Git
  objects; do not claim that those binaries emitted it.

The second operation takes a bounded read-only snapshot of a primary database
using the existing administrative identity. It checks the database name and
unfiltered reader authority, holds an access-share lock on the regular tracking
relation, and compares recorded filenames with the expected prefix. It permits
existing work without reading or changing delivery payloads. No migration,
role provisioning, retry, drain, schema repair or delivery transition occurs.
Do not put the connection in command-line arguments or logs. The installation
must independently bind this connection to its original database resources.

Reports use `ai-sdlc.upgrade-migration-observation/v1` and distinguish:

| State | Observation |
| --- | --- |
| `source-recorded` | Recorded filenames exactly match the supplied source prefix. |
| `partial-migration-recorded` | A strict intermediate prefix is recorded. Earlier files may have committed before a later failure. |
| `target-recorded` | Recorded filenames match the full embedded target catalog. |

Missing, extra, renamed or out-of-order tracking entries reject. A source equal
to target yields `source-recorded` with no remaining migrations. None of these
states proves installed image identity, readiness, historical SQL hashes, schema
integrity or compatible consumers. The report explicitly marks schema/hash
verification, producer exclusion, upgrade/rollback authorization and workflow
evidence false. Concurrent producers are not excluded by a read-only snapshot;
an observation cannot authorize a later migration.

The regression matrix applies real N-1 to N migrations in disposable PostgreSQL
with original pending/completed work and outbox contents retained. An injected
failure in the second pending file proves that the first committed migration
remains recorded while the failed file's DDL and tracking insert roll back.
This tests per-file recovery behavior, not a live installation upgrade. Runtime
owned draining, verified backup restoration, bounded rollout and compatible
rollback still require their own implementation and real validation under
[issue #441](https://github.com/os-santiago/ai-sdlc/issues/441).

## Hold database write exclusion during an administrative pause

The migrator's `hold-upgrade-fence` operation is an installation-only streaming
protocol. It is not a workflow endpoint, an upgrade executor or a Kubernetes
drain receipt. The supported initial profile is explicit: source schema 0033 or
0034, preserved work before candidate production, no downstream delivery history,
no claimed execution, unacknowledged executor effect, active model budget/call or
incomplete cleanup. A stopped execution with an uncertain failure reason and
confirmed cleanup can remain in history. Queued work, admissions and completed
records in the supported aggregates are retained. Unsupported histories reject
without deleting or resetting them; broader support requires additional proof.

The original primary database and administrative role are checked **before** any
lock. One bounded transaction takes the same advisory lock as `Migrate`, then
SHARE locks on every table in the reviewed source inventory. A fresh READ
COMMITTED inspection follows all locks. This is an administrative read/write
transaction that changes no workflow rows, not a read-only observation. A busy
or unsupported history causes immediate release so the original controllers can
continue reconciliation. Never wait for cleanup while holding its write locks.

Send exactly one private JSON line containing `databaseURL`, `database` and the
publication-bound `sourceCatalog`. The connection must be bound independently
to the installation's original database resources and never placed in arguments
or logs. The initial JSON response has version `ai-sdlc.upgrade-fence/v1`, state
`held`, an original `nonce`, and `expiresAt` in Unix milliseconds. Subsequent
lines contain exactly `operation` (`check` or `release`) and that same `nonce`.
Checks do not extend the original deadline. There are at most 256 commands, a
120-second database fence and a 125-second total protocol deadline. EOF, malformed
frames, wrong nonce, deadline, blocked output or lost database custody aborts the
session. No automatic reacquisition occurs. `released` is emitted only after the
original transaction's rollback acknowledges releasing its locks.

While the original session remains held, an installer must independently:

1. Validate its exact source configuration and original Kubernetes identities.
2. Confirm the absence of execution/cleanup resources; SQL exclusion alone does
   not attest Kubernetes containment.
3. Bracket each pause operation with a successful `check`, using its original
   finite transition deadline and aborting on lost custody.
4. Observe disappearance of the original producer Pods and execution resources.
   Zero desired replicas or a failed readiness check is insufficient.
5. Release the original fence only after that observation. Never start migration
   Jobs while the fence is held, and never infer that a failed/ambiguous pause
   succeeded. Preserve the transition for explicit reconciliation.

The protocol always reports `workflowRowsWritten=false` and
`upgradeAuthorized=false`. It does not validate a backup, artifact trust,
configuration compatibility or rollout recovery. Its real PostgreSQL regression
suite proves writer/migrator/replica exclusion, preserved rows, fresh state after
lock waits, backend-loss cancellation and bounded private I/O. Kubernetes pause
integration and the remaining upgrade gates are still required before live use.

## Trust and evidence limits

This resolver trusts the authenticated GitHub API and the reviewed main-branch
publication workflow. Artifact integrity and matching metadata do not prove
that Quay still serves the image, validate a signature, establish trusted-builder
provenance, or demonstrate a deployment. Actual installer pulls and observed
runtime image identities are separate evidence. Konflux/Conforma verification
remains required by the enterprise promotion roadmap; this helper grants no
SLSA level and does not replace that gate.

The helper does not enable AI-SDLC execution, modify issue eligibility, dispatch
work, create releases or advance business state. Installation readiness and a
successful shadow upgrade remain separate from issue-to-production delivery.

## Pause an original pre-candidate runtime

`./install.sh runtime-upgrade pause` is a bounded administrative operation for a
complete initial activation whose retained history has not reached candidate
creation. Select the original cluster and exact successful publications:

```bash
./install.sh runtime-upgrade pause --cluster ORIGINAL_CLUSTER --provider podman \
  --source-run SOURCE_RUN_ID --source-revision FULL_SOURCE_COMMIT_SHA \
  --target-run TARGET_RUN_ID --target-revision FULL_TARGET_COMMIT_SHA
./install.sh runtime-upgrade status --cluster ORIGINAL_CLUSTER --provider podman
```

The target publication must include `describe-migrations` and
`hold-upgrade-fence`. Its migrator acts only as an administrative helper; this
command keeps the original installed component images and does not migrate the
database. The current helper supports source catalogs 33 and 34 only. It rejects
candidate/downstream history, live execution claims, incomplete cleanup,
unacknowledged execution effects and active model calls or budgets.

The installer verifies the original activation, resource UIDs, database Pod,
private credential custody and unchanged chart. With the original database
session holding bounded write exclusion, it freezes both policies, pauses issue
intake and scales the original producer Deployments to zero through Helm. It
retains the original policy expiry and authority. Completion requires the
original producer Pods and execution resources to be absent, the fence release
to be acknowledged, helper cleanup to finish and the paused installation to be
observed again. No workflow rows or delivery-stage receipts are created.

A separate private `runtime-upgrade-pause.json` records intent before the Helm
send. A lost response or partial operation stays incomplete; invoking the command
again does not replay an uncertain send. Preserve that record and original
installation for reviewed recovery. After a completed pause, invoking `pause`
again only reobserves the result and repairs its installation snapshot if needed.
`status` is read-only. This operation does not provide resume, migration,
backup/restore, rollback or an end-to-end delivery result. Those gates remain
required before installing new runtime images.

## Restore-verify a private backup of the paused runtime

After the original pause has completed, the same entrypoint can capture and
restore-verify a bounded backup:

```bash
./install.sh runtime-upgrade backup --cluster ORIGINAL_CLUSTER --provider podman
./install.sh runtime-upgrade backup-status --cluster ORIGINAL_CLUSTER --provider podman
```

The command uses the original recorded pause plan. It rechecks the paused
controllers, database Pod and credentials, then holds the reviewed SQL fence
while exporting the `ai_sdlc` database and global roles. It writes an exclusive
private directory and mode-0600 files; each archive is limited to 4 MiB for this
local pilot. Existing or partial archives are never overwritten or adopted.

A fresh owned PostgreSQL container uses the original immutable database image,
no network, ephemeral data storage and read-only archive mounts. Restoration
must succeed without ignored errors. The verifier compares every public table's
row count and ordered row hashes, role attributes/password hashes, memberships,
role settings and the complete schema dump, including ownership, privileges,
functions and row-level security. Only the bootstrap grantor of built-in role
memberships is canonicalized between fresh clusters. A deterministic restrict
key makes the schema comparison repeatable; that SQL is only hashed and discarded,
never executed. The archive and globals used for restoration retain their normal
format. See PostgreSQL's [pg_dump](https://www.postgresql.org/docs/17/app-pgdump.html)
and [pg_dumpall](https://www.postgresql.org/docs/17/app-pg-dumpall.html) documentation.

Completion requires removing the original restore container and rechecking that
the paused source still matches the captured backup. The private record is
`runtime-upgrade-backup.json`; archives stay under `runtime-upgrade-backup/` for
recovery review. A missing container acknowledgement or incomplete capture stays
incomplete and is not automatically replayed. Preserve the original intent.

`backup-status` is read-only and reports restoration of the recorded backup at
`verifiedAt`; it does not assert that later database changes match it. A future
upgrade must revalidate the current source against that proof. Neither command
migrates or restores into the installed database, reactivates producers, grants
rollback authority or validates autonomous delivery. This bounded local backup
is not a replacement for an enterprise retention or disaster-recovery service.

## Validate the diagnostic migration on a disposable database

Before implementing an installed-runtime transition, validate the published
33-to-34 diagnostic migration against the previous published migrator:

```bash
python3 tools/validate_upgrade_migration.py --provider podman \
  --source-run SOURCE_RUN_ID --source-revision FULL_SOURCE_COMMIT_SHA \
  --target-run TARGET_RUN_ID --target-revision FULL_TARGET_COMMIT_SHA \
  --output NEW_PRIVATE_EVIDENCE_DIRECTORY
```

This harness authenticates both publications, initializes an owned ephemeral
PostgreSQL database with the source migrator, retains fixture work and outbox
history, restores its backup into a separate database, and runs the target
migrator. It compares the original rows, migration tracker, schema definitions,
role identities and semantic ACL inventory, including types, default privileges,
tablespaces, languages, foreign objects, large objects and parameter ACLs. ACL
ordering is ignored; owners, grantors, grantees, privilege types, grant options
and the distinction between default and explicit ACLs are retained. All original
component credentials must authenticate with SCRAM, and an incorrect password
must fail. Negative controls verify detection of changed table, type and default ACLs,
and changed role authority.
Only the worker receives the reviewed diagnostic privileges.

Each migrator uses a persisted creation intent and original container ID before
starting SQL. Component credentials travel through bounded private stdin; the
database URL is inherited by environment name. Missing acknowledgements and
incomplete intents cannot trigger an automatic replay or adoption by name.
The sanitized report distinguishes this disposable compatibility proof from
installed-runtime validation and autonomous E2E delivery. No production CLI,
runtime migration, restart or rollback authority is provided by this harness.


## Apply the bounded diagnostic runtime transition

After a completed pause and verified backup, the same installer can apply the
reviewed additive `0033` → `0034` diagnostic bridge:

```bash
./install.sh runtime-upgrade diagnostic --cluster ORIGINAL_CLUSTER --provider podman
./install.sh runtime-upgrade diagnostic-status --cluster ORIGINAL_CLUSTER --provider podman
```

This command uses the exact publications from the original pause plan. It
requires contained failed work only, stopped execution requests, and no pending
authenticated deliveries or inbox results. A pending queue is a refusal, not a
request to consume or delete rows. The old unsigned outbox cannot authorize work.
The current database must still match the restored backup before migration.

The published migrator runs once with the original role credentials. Afterwards,
the shared compatibility verifier requires unchanged historical rows, tracker
entries, schema, role identities and ACLs, plus the worker-only diagnostic
privilege boundary. The new diagnostic table must be empty. A missing migration
acknowledgement cannot cause a retry. Its original deadline covers container
creation, custody checks and SQL execution; owned cleanup remains available after
expiry.

The installation then changes only the worker image and its two gateway
consumers: `worker/worker`, `worker/model-gateway` and
`verifier/model-gateway`. The verifier's main image, executor/reviewer placement,
release images, credentials, trust and approval expiry remain original. The
private record explicitly identifies the source publication and two selected
target artifacts; this is not a uniform installation of the target publication.
All resource identities and unrelated rendered fields must remain unchanged.

The installer stages the image delta with producers paused, starts downstream
components with frozen policies, checks the original release prerequisites, and
arms downstream components while intake remains stopped. It opens intake only
after the preceding phase is completely observed. The last Helm change is only
the intake replica count. There are no application issues, stage requests,
candidates or release receipts generated by this administrative operation.

`diagnostic-runtime-upgrade.json` retains each intent before effects. A lost
Helm acknowledgement permits observing an already-complete target; it never
replays an uncertain send. Preserve any partial or unknown state for recovery
review. `diagnostic-status` observes the recorded phase without repairing it.

To recover the original component selection while the original approval window
and retained-history proof still hold:

```bash
./install.sh runtime-upgrade recover-original --cluster ORIGINAL_CLUSTER --provider podman
```

Recovery rejects unresolved helper cleanup and incomplete target observations.
It acquires the reviewed SQL fence, returns the original controller templates to
a fully paused state, verifies original resource identities and history, then
reactivates the original components with intake last. The additive schema and
all history are retained; this is not a database downgrade or restoration over
live data. New work, candidate creation, changed history, unknown partial states
or expired approval require their own recovery procedure and cannot be silently
adopted. The original activation and backup custody records remain unchanged.

For the disposable phase fixture, add `--exercise-transition` to
`tools/validate_upgrade_pause.py`. It uses inert Helm producers to test actual
pause/reactivation, lost-ack observation without replay, fenced recovery and
intake ordering. Combine that result with the published PostgreSQL migration
harness and exact chart-render checks. These fixtures do not prove the installed
CLI or autonomous issue-to-production delivery; verify those in their respective
environments before claiming success.

## Apply the reviewed generator tool budget

For an already completed tool-budget transition whose test-environment approvals
need a new finite window, use the separate [explicit reapproval procedure](approval-window.md).
It cannot renew an incomplete upgrade or an existing workflow request.

An installation that completed the diagnostic bridge can adopt the calibrated
generator profile through the same entrypoint:

```bash
./install.sh runtime-upgrade tool-budget --cluster ORIGINAL_CLUSTER --provider podman
./install.sh runtime-upgrade tool-budget-status --cluster ORIGINAL_CLUSTER --provider podman
```

This closed transition changes only the generator's SCC tool-step limit from 16
to 24. It preserves 16 provider calls, the recorded generation time (600 seconds
unless the installation selected up to 900 with `--generation-seconds`), the
recorded requested output tokens per response (4,096 unless the installation
selected up to 8,192 with `--generation-answer-tokens`), every byte quota, reviewer profile, image, placement, credential and
original trust expiry. The chart derives new configuration checksums for its
controllers; no checksum is ignored during observation. Restoring the single
candidate configuration value must reproduce the original active render exactly.

The source must be the complete recorded diagnostic activation, on catalog 34,
with contained failed work, stopped execution requests and no pending
authenticated deliveries. A database write fence protects the pause. The
source's unchanged schema, roles, ACLs and original migration records must match
the prior diagnostic proof. The additive diagnostic table must also retain its
reviewed columns, constraints, indexes, triggers and exact enabled/forced row
security policy. Existing authority drift is rejected rather than adopted.
The initial full snapshot is a preserved preliminary observation: the active
poller can still refresh its cursor and per-issue observation. Immediately before
the first pause, under the write fence, all other table hashes and both polling
table cardinalities must match. Polling identities must match the unchanged
issue heads and revision payloads. Only then is a separate `fencedSourceProof`
saved together with the first pause intent. The original snapshot, nonce and
deadline remain unchanged. This capture is unavailable after any pause, backup,
recovery or Helm transition intent. Every subsequent comparison includes the
complete polling tables without exceptions.

The installer rechecks original database authority while that fence is held,
before persisting a Helm intent or sending a change. It then creates a new
private backup, restores it into an isolated PostgreSQL container, compares it
and confirms cleanup. Existing failed attempts and diagnostic rows are retained.
This operation performs no SQL migration, workflow retry or application delivery.

After backup verification, the installer stages the profile with producers
stopped, starts downstream controllers with frozen policies, checks original
release prerequisites and retained data, then opens intake last. Work requested
afterward must enter through issues and satisfy the original authority policies.
The operation has its own bounded administrative deadline; it does not renew
the source trust or any execution allocation.

`tool-budget-upgrade.json` and `tool-budget-backup/` are separate from the
original diagnostic, pause and backup records. Partial backup or pause outcomes
are not overwritten or replayed. A missing Helm acknowledgement permits observing
an already-converged target, never sending the same uncertain transition again.
The status command only observes. Preserve these records when a command stops.

For a completed observed phase with unchanged retained history and resolved
helper cleanup, recover only the original 16-tool profile with:

```bash
./install.sh runtime-upgrade recover-tool-budget --cluster ORIGINAL_CLUSTER --provider podman
```

Recovery uses the original write fence, identities and intake-last sequence.
It does not restore over the database, remove diagnostics or revert software
delivery. New work, changed history, expired trust or an unresolved partial
Kubernetes state require a separately reviewed recovery procedure.

The disposable migration harness accepts `--exercise-tool-budget-history` to
test restoration of two failed attempts and a retained exit 22 diagnostic.
The disposable Helm harness accepts `--exercise-tool-budget-transition` to test
the new phase ordering, lost acknowledgements and original-profile recovery on
catalog 34. These are isolated component proofs; they do not establish that the
installed CLI or autonomous delivery has succeeded.
