# Install the signed build capability

This capability connects the release-manager's immutable merged-record selector,
the signed relay, a dedicated build-controller process and the closed Tekton
build/publish transport. It is limited to `os-santiago/saferus` and
`ai-sdlc-production` on an existing, installer-owned Konflux kind cluster.
A built record is not authorization to release or deploy. The autonomous
issue-to-production demonstration remains pending.

## Prerequisites and desired state

Start with the [frozen runtime](frozen-konflux-runtime.md). The build namespace,
original single-use storage slots, admission policies, registry credential,
trusted CA bundles and separately approved immutable platform trust must already
exist. Use the production [initialization](build-bootstrap.md) and
[observation](build-profile-observation.md) operations. Disposable fixture reports
are never operational installation inputs: their namespaces and slots are deleted.
Use the [persistent foundation entrypoint](build-foundation.md) to provision
these original resources and produce a separately approved public bundle. Do not create an eligible demo issue until the required chain
and its recovery behavior are installed and validated.

Keep a reviewed public JSON bundle alongside the installation configuration:

```json
{
  "version": "ai-sdlc.build-desired/v1",
  "revision": "<exact reviewed AI-SDLC commit from images.lock.json>",
  "controllerDigest": "sha256:<published build-controller image digest>",
  "profile": {"<complete approved observed profile>": "..."},
  "slots": [{"<complete original slot identity>": "..."}],
  "issueMaxAgeSeconds": 60
}
```

The illustration deliberately contains placeholders and cannot be installed.
Select the controller image from the successful exact-main-revision container
publication's `supply-chain-image-build-controller` artifact. The regular image
lock and controller must come from the same reviewed revision. All registry
images use immutable digests. Never substitute a PR image or a local fixture
binary for a published component.

The `LocalInstallation` configuration uses `platform: konflux`,
`repository: os-santiago/saferus`, `environment: ai-sdlc-production`,
`profile: verification`, `independentReview: true`, `publication: true`,
`merge: true`, and `buildBundle: ./approved-build.json`. Paths resolve relative
to that configuration. Credentials are explicit CLI choices, not JSON fields.

```sh
./install.sh plan --config ./reviewed-installation.json
./install.sh local --config ./reviewed-installation.json \
  --github-auth gh-cli --codex-auth-file /private/path/auth.json
./install.sh status --config ./reviewed-installation.json
```

`plan` checks the declared scope and bundle shape without credentials or live
cluster observation. It does not certify that the build profile is current.
`local` requires an already frozen runtime, then runs the pinned production
migrator's read-only profile observation using the original kind node network
namespace. Any difference from the approved profile aborts before authority is
activated. It does not relearn or silently approve drift. The compiler binds
explicit inner/outer policy revisions, original slots and the Go-derived intake
configuration digest; private API input travels only through stdin.

## Runtime boundaries

Helm installs a separate non-root, read-only-root-filesystem controller with its
own signing identity and database role. Its projected Kubernetes credential can
create/read/list/delete PipelineRuns only in `ai-sdlc-builds`; the remaining
permissions are finite inventory reads needed to validate the approved platform
and original storage. It cannot create Pods, execute into them, patch workloads,
change admission or mutate namespaces. Only the registry-auth Secret may be read
for its committed fingerprint; no model or GitHub credential is mounted there.

The controller is therefore part of the registry credential trust boundary: its
read permission could access the credential itself, not merely a server-side
fingerprint. Only the publisher workload receives that Secret as a mounted
volume; the source build workload receives no registry credential.

The dedicated NetworkPolicy accepts the relay on port 8088 and permits DNS,
PostgreSQL and the approved finite API/registry targets. It remains present when
the chart-wide NetworkPolicy option is disabled. The general runtime policy
excludes this controller, so it cannot widen these rules.

The migrator provisions build-specific database privileges only when the complete
verification/review/publication/merge capability is selected. The release-manager
selects signed immutable merge facts; neither an operator nor an issue can supply
a source SHA, recipe, workload body, image destination or stage-advance command.
The relay loads matching current loop policies before delivery. Build execution
uses a dedicated SQL session lock, durable effect intents, original workload UIDs
and containment before new work. Unknown create outcomes quarantine the merge and
physical slot; a workspace is never automatically reused or erased.

Build upgrades and teardown are not automatic: the installer rejects any
existing enabled build capability, including a frozen one, until a dedicated
containment-preserving upgrade procedure is implemented. It also rejects an active
innerloop runtime. Preserve controller, credentials and storage during containment. Freeze
through reviewed authority configuration and prove archived cleanup complete
before changing privileges or removing capability resources. Current runtime
status is observable without requiring the deployment to be frozen.
## Initial empty runtime preparation

Before installing additional capabilities, an existing owned Saferus Konflux
runtime can be excluded from producing work through the single installer:

```sh
./install.sh prepare --cluster ai-sdlc-konflux-reviewed \
  --provider podman --images /path/to/reviewed/images.lock.json
```

Prerequisites are the current reviewed image set and schema already installed
through the normal maintenance installation, frozen Saferus authority, and no
delivery history. The migrator image must support `inspect-empty-bootstrap`.
An older schema is rejected; that rejection is not evidence of existing work.
Do not erase history or bypass the inspection to make preparation pass.

The installer holds the existing installation lock. It compares effective
Deployments and admitted Pods with the reviewed Helm render, checks each role's
database custody, and binds the primary database to its Secret, Service,
Endpoint, Pod, StatefulSet, PVC and PV. It checks for other producers and
execution/cleanup workloads before observing the database. The migrator uses a
bounded read-only snapshot, requires RLS-independent visibility and rejects any
workflow row, including completed history, as well as unknown or missing tables
and migrations. No SQL is supplied by the operator.

After recording original identities and its intent, the installer reapplies
the same Helm desired state with `runtime.paused: true`. The chart requires
explicitly frozen policies and renders every delivery Deployment with zero
replicas. PostgreSQL and the schema migrator remain available. Preparation
waits for producer Pods to disappear and repeats the database and authority
checks. A snapshot alone does not exclude concurrent producers.

`bootstrap-preparation.json` records this administrative state independently
from workflow records. Reapplication must match the original inputs and
identities. Drift or uncertain completion leaves the installation frozen;
there is no automatic rollback or unpause. Ordinary `install.sh local` refuses
to resume a recorded preparation. This command is limited to initial setup;
upgrades with history require the separate containment-preserving lifecycle
procedure. Registry ACLs, independent signing trust, provenance verification,
promotion and deployment remain separate required installation gates.

The successful result is **paused and empty**, not ready for autonomous
delivery. No issue is admitted and no workflow stage is advanced by preparation.

After `prepare`, continuation is not implemented yet: [issue #574](https://github.com/os-santiago/ai-sdlc/issues/574) tracks the remaining capability transition. The earlier `local --config` example applies before preparation only. Do not delete the preparation record or unpause the release manually. This command currently ends at a frozen, empty installation for administrative integration checks.

If initial inspection is rejected, the installer reports a finite boundary such as `connection`, `primary-authority`, `table-inventory`, `migration-inventory` or `history`. These identify where validation stopped, not permission to bypass it. A history-boundary failure can also be a read failure; it does not itself prove rows exist. Connection diagnostics may further distinguish configuration, authentication, missing database, permission, capacity, unavailable server, timeout or network failure using fixed categories derived from driver types and known SQLSTATE codes. An unknown cause remains `connection`. Driver messages, connection strings and row contents remain private. Keep the runtime frozen and resolve the condition through reviewed installation changes.

### Cold installation with a Helm-owned registry

Konflux v0.2.2 continuously reconciles its embedded Zot configuration and does
not expose native ACL configuration. For the Saferus demonstration, select the
following profile when creating a **new** cluster:

```sh
./install.sh supply-chain local --cluster ai-sdlc-demo --provider podman \
  --managed-registry --images ./reviewed-images.lock.json
```

The reviewed image lock must include a migrator implementing
`create-registry-custody`. This profile also requires the pinned Python packages
in `tools/quality-requirements.txt`. Reapply with exactly the same command and
image lock, before creating either `ai-sdlc-runtime` or `ai-sdlc-builds`. Once either namespace exists, this profile allows only status; runtime freeze is not proof of absent workflow history. An existing operator-owned registry cannot be converted by this
command; keep it intact and choose a new cluster name.

The installer disables only Konflux's internal-registry subcontroller during
cold creation. A separate Helm release declares Zot at the existing
`registry-service.kind-registry` endpoint, native repository ACL, restricted
workload, private network boundary, certificate and trust-manager Bundle. The
publisher can read/create/update only `ai-sdlc-fixtures/saferus`; the provenance
reader can only read it. Neither can delete. Credentials are generated once
through the pinned migrator and stored in protected local custody and immutable
Kubernetes Secrets, outside Helm values and history.

A two-GiB retained PVC replaces ephemeral registry storage. Reapplication checks
original cluster, Konflux parent, namespace, Secrets, Helm resource and storage
identities; replacements are not adopted. A complete lost-create response may
be recovered from its original recorded nonce and bytes. An incomplete Helm
creation is retained for inspection, not overwritten automatically. This is an
initial-installation profile, not a procedure for upgrading installations with
workflow history.

Installation creates one explicitly administrative OCI marker and verifies real
TLS/hostname, publisher writes, reader reads, write/delete/cross-repository
and anonymous denials, followed by another successful read. The deletion probe
uses a confirmed absent digest, never retained software. This marker is not a
build, signature, provenance receipt, release or autonomous workflow evidence.
The endpoint's local CA check does not approve a provenance signing key.

Registry storage remains local to the kind node. Retain protects against PVC
release deletion; it does not provide high availability, off-node backup or
recovery after deleting the cluster. Pod-replacement persistence and Konflux
reconciliation must also pass the real installation acceptance check before
this profile is used for delivery.

Before installing the runtime/build namespaces, execute the same installer's cold
acceptance procedure:

```sh
./install.sh supply-chain verify-registry --cluster ai-sdlc-demo --provider podman \
  --managed-registry --images ./reviewed-images.lock.json
```

It reapplies the same reviewed chart, records the original registry Pod and
storage identities, deletes only that Pod with a UID precondition, and waits for
a healthy replacement. The original administrative marker must still exist;
the probe refuses to recreate missing persistence evidence. It also observes
configuration after annotating the existing Konflux parent. Evidence remains in
protected installation state and does not advance any workflow. Repetition uses
the retained result plus fresh ACL checks; it does not repeatedly delete Pods.

To reproduce role provisioning and inspection safely, run `python3 tools/validate_bootstrap_inspection.py --provider podman --output /path/to/new-evidence-directory`. It creates and removes its own PostgreSQL instance; it never contacts the installed database.
