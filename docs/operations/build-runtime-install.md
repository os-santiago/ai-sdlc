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
Provisioning this persistent foundation through the entrypoint remains a separate
installation task. Do not create an eligible demo issue until the required chain
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
