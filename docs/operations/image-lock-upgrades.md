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
