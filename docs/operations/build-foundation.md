# Prepare persistent closed-build infrastructure

`install.sh build-foundation` prepares administrative infrastructure on an existing
installer-owned Konflux kind cluster with a frozen AI-SDLC runtime. It never
creates an issue, signs a workflow request, writes workflow database state,
activates delivery, or releases software. Each storage slot is single-use.

Use a reviewed main container publication that contains the production migrator
observers/compiler, artifact-builder and build-controller. Resolve all identities
from the same successful publication:

```sh
python3 tools/install_images.py --include-build --run RUN_ID \
  --revision FULL_REVIEWED_MAIN_SHA --output ./reviewed-images
```

The normal `images.lock.json` remains compatible with existing installations.
`build-images.json` additionally records the exact published publisher image and
controller digest; `evidence.json` binds artifact IDs, archive digests, workflow
origin and source revision. Select those two values explicitly below. Do not use
illustrative or locally retagged image digests.

```sh
./install.sh build-foundation plan --cluster ai-sdlc-konflux-reviewed \
  --images ./reviewed-images/images.lock.json \
  --publisher-image quay.io/os-santiago/ai-sdlc@sha256:PUBLISHED_PUBLISHER_DIGEST \
  --controller-digest sha256:PUBLISHED_CONTROLLER_DIGEST
./install.sh build-foundation local --cluster ai-sdlc-konflux-reviewed \
  --images ./reviewed-images/images.lock.json \
  --publisher-image quay.io/os-santiago/ai-sdlc@sha256:PUBLISHED_PUBLISHER_DIGEST \
  --controller-digest sha256:PUBLISHED_CONTROLLER_DIGEST
```

The first local call creates only the finite desired foundation:

- A restricted `ai-sdlc-builds` namespace, dedicated service accounts, network
  policies, resource defaults and explicit immutable optional CA bundle objects.
- One original PVC/PV with `Retain` reclamation and a root-owned dedicated 4 GiB,
  32,768-inode tmpfs on the original kind node. The normal published migrator
  initializes custody and a UID/GID 1000 workspace. Its executable is extracted
  from the pinned image and hash-checked after copying to that node.
- An immutable copy of the installed Konflux registry client credential, mounted
  only in the publisher workload. No credentials are printed or stored in public
  manifests. The controller remains part of the credential trust boundary through
  its read-only dependency check.
- Native validating/mutating admission definitions compiled by the production
  migrator from the actual original slot and observed API/registry targets.

The declarative foundation manifest is
[`build-foundation.json`](../../runtime/k8s/deploy/konflux/build-foundation.json).
The runtime workloads remain managed by the main Helm chart. Root node
initialization is an explicit administrative installation operation; no runtime
component receives that permission.

## Review trust separately

The installer writes `build-trust-candidate.json` under the cluster's private
state directory and returns `awaiting-trust-review`. Compare the controller,
network implementation, configuration and complete mutating admission inventory
against the reviewed platform installation. Observation alone is not approval.
After the independent administrative review, save the accepted public document
as a separate immutable review artifact, then repeat the same command with
`--approved-trust /path/to/reviewed-build-trust.json`.

The approval is loaded before effects. Aliases and hardlinks to mutable installer
outputs are rejected. A changed current inventory is denied, never automatically
learned. Once approved, an immutable `build-trust` ConfigMap and a public
`build-desired.json` bundle bind the exact profile, original slot and image
revision. Use that bundle with the
[build runtime installation](build-runtime-install.md). Foundation preparation
alone does not prove controller readiness or issue-to-production E2E.

## Reapply, status and recovery

The cluster's installation lock excludes concurrent administrative reconciles.
A durable create intent precedes each API POST. Reapply requires the same desired
body, ownership nonce and original UID. A lost create reply may be reconciled only
to that exact resource; absence, deletion, replacement or drift is rejected without
another create. Node ID, Kubernetes UID and boot identity are checked before
mutating steps. Mount identity is retained; a missing mount is never recreated.
Complete custody initialization preserves existing data and does not make a
previously used slot reusable.

`build-foundation status` accepts the same image/scope arguments, checks recorded
resource UIDs, nonces and effective desired state plus cluster/node identity, and
reports the **recorded preparation status**. It explicitly does not certify full
current profile readiness. Preserve the reviewed image lock for repeat commands.

State is mode 0600 under `~/.local/state/ai-sdlc/CLUSTER/build-foundation.json`;
API/registry credentials are never written into it. Interrupted or partial setup
retains its original evidence for diagnosis. There is no automatic teardown,
forced finalizer removal, resource adoption, storage erasure or repair. Once build
is enabled, foundation mutation is refused, even if authority is subsequently
frozen. A containment-preserving upgrade/reclamation procedure remains required.
This first implementation supports one node and one bounded slot, with tmpfs loss
on node reboot; it is a test-platform capability, not HA or production durability.

## Validation

The isolated node test uses a published migrator inside an offline disposable
container. It proves initialization, data-preserving reapply, mount-ID rejection,
absence quarantine and extractor cleanup. The real Kubernetes journal fixture
proves lost-ACK recovery to an original UID, drift rejection and no recreation
following deletion. Both are explicitly administrative fixtures; they do not
simulate or claim autonomous delivery.
