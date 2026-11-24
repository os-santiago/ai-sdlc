# Configure an empty, paused runtime

`install.sh local --configure-prepared` installs the reviewed verification,
review, publication, merge, build and provenance capabilities while retaining
zero replicas for all nine delivery Deployments and frozen inner/outer policies.
It does not admit issues, activate delivery or produce release evidence.

Use this only after the same installation passed
[empty runtime preparation](build-runtime-install.md#initial-empty-runtime-preparation).
Keep the original preparation, platform, build-foundation and registry records.
Install and accept the [managed registry](build-runtime-install.md#cold-installation-with-a-helm-owned-registry)
before creating runtime/build namespaces. Provision the
[original build foundation](build-foundation.md) and independently approve its
public trust. No completed or failed workflow history is accepted by this path.

## Reviewed inputs

Select an exact successful main-branch [image publication](image-lock-upgrades.md)
with `tools/install_images.py --include-build`. The output must retain
`images.lock.json`, `images.evidence.json` and `build-images.json` together.
The separate provenance trust document must be reviewed against the original
cluster and build namespace, registry CA, signing public key, retained publisher
image and newly selected verifier image. Observing a key does not approve it.
The migrator validates this public document before any registry credential is
copied; it cannot issue a provenance result or sign a release.

```sh
./install.sh local --platform konflux --cluster ai-sdlc-demo --provider podman \
  --repository os-santiago/saferus --environment ai-sdlc-production \
  --profile verification --independent-review --publication --merge \
  --images /reviewed/publication/images.lock.json \
  --build-images /reviewed/publication/build-images.json \
  --build-bundle /private/installation/build-desired.json \
  --provenance-trust /reviewed/saferus-provenance-trust.json \
  --configure-prepared --github-auth gh-cli \
  --codex-auth-file /private/codex/auth.json --timeout 10m
```

Paths are illustrative and must refer to the actual original installation and
reviewed inputs. Credentials remain explicit private inputs, never Helm values,
Git files or command-line token strings. `gh-cli` imports the authenticated
client; it does not mint a narrower token. Credential rotation is excluded from
this initial transition.

When the runtime publication advances, the installer derives the controller
revision from the reviewed `build-images.json`. It requires the supplied original
bundle's digest to match `build-foundation.json`, preserves the publisher image,
profile and physical slots, and reobserves them with the new migrator. It does not
rewrite foundation custody or authorize a replacement publisher/storage slot.

## Exclusion, recovery and evidence

The per-cluster lock covers the operation. A protected
`prepared-configuration.json` records original inputs, rendered desired state,
private credential create intents, nonces and UIDs before effects. The same
registry reader credential is copied into its dedicated immutable runtime
Secret; publisher credentials never become provenance-reader credentials.
Registry configuration, Helm ownership, original storage and reader source are
checked in read-only mode. Existing component identities and database passwords
are retained.

The installer compares the paused workloads, policy/configuration, Helm-owned
ServiceAccounts, RBAC, Services and NetworkPolicies with persisted reviewed
renders. It rejects additional runtime security resources, missing resources,
changed original UIDs and changed injected platform trust. A completed Secret
POST whose response was lost can be recognized only by its original nonce and
bytes. Missing objects after recorded creation are not recreated.

Only a complete recorded before or after state is eligible for continuation.
A partially applied Helm release or migration prefix stays paused and fails
closed. Migrations commit per file; this procedure does not promise a single
transaction across schema and Kubernetes changes. Keep the original records and
resources for the separately reviewed containment-preserving recovery procedure;
do not delete records, rotate secrets or unpause workloads to force progress.
A durable intent persisted in the read-only window before any effect — phase
`preparing` with no target and no journaled resources, private credentials or
bound sources — may be abandoned for a newly reviewed request only after the
cluster is re-proven equal to its recorded source; the installer then captures a
fresh before-state under the new request digest. Any recorded credential, bound
source or applied target keeps the intent permanently fail-closed.
The existing offline-sandbox installer retains its own custody and conformance
controls; the preparation boundary is checked before and after it runs.

After Helm and migration completion, the new migrator proves the database is
still empty. Current original build profile, registry custody and public trust
are checked again. The result is `prepared-paused`, with `deliveryEnabled: false`
and `liveWorkflowAdvanced: false`. These checks establish configuration custody,
not running component readiness, an autonomous E2E, an SLSA level or production
acceptance. Activation, promotion/deployment and recovery remain separate gates.

For the additional opt-in release capability and its tenth paused producer, see [deployer installation](deployer-installation.md). Supply the reviewed release target and same-publication deployer pins together; the original preparation custody remains mandatory.

Once all prerequisites are installed, use [finite initial activation](initial-runtime-activation.md). The activation record preserves the original paused configuration and enables issue ingress last.
