# Install AI-SDLC with Helm

The supported first installation target is **Linux amd64 + local kind**.
The same chart renders Kubernetes resources; installing into an existing
remote cluster is a separate, not yet validated onboarding path (#434).
The installer needs Python 3.11+, Git, Internet access and a working Docker
or Podman installation. Allow at least 4 CPUs, 6 GiB available RAM and
15 GiB free disk for kind, PostgreSQL and image downloads. These are initial
planning allowances, not measured production sizing.

The installer checks Linux inotify limits before creating a cluster. If it
reports a low limit, run `sudo ./install.sh host --configure-host` once and
retry the installation as your normal user. This explicit command raises
limits to the committed minima in
[`host-prerequisites.json`](../runtime/k8s/deploy/local/host-prerequisites.json),
following the [kind inotify guidance](https://kind.sigs.k8s.io/docs/user/known-issues/#pod-errors-due-to-too-many-open-files).
It preserves higher limits, verifies the result, and affects only the current
boot. Use your host configuration manager for persistence across reboots.
`./install.sh host` only checks; the installer never invokes sudo itself.

```bash
git clone https://github.com/os-santiago/ai-sdlc.git
cd ai-sdlc
./install.sh plan
./install.sh local
./install.sh status
```

The same `install.sh` can be downloaded and executed outside a checkout. It
clones this repository into `~/.local/share/ai-sdlc/source` before delegating
to the installer. `AI_SDLC_SOURCE_DIR` selects a different checkout location;
`AI_SDLC_INSTALL_REF` selects a reviewed commit/tag for a new clone. When a
checkout already exists, a different requested revision fails explicitly:
review and update that checkout rather than silently replacing local work.

Use `--provider podman` or `--provider docker` to select the container
runtime explicitly. `--cluster ai-sdlc-dev` creates a separate named
installation; the installer records and verifies the cluster's `kube-system`
UID and refuses to adopt an unrelated or externally recreated cluster.
Rootless runtime support depends on host configuration; the installer never
changes host privileges or silently retries as root. See the
[kind rootless prerequisites](https://kind.sigs.k8s.io/docs/user/rootless/).

The script downloads pinned kind, kubectl and Helm clients into the user's
cache and verifies archive **and extracted binary** SHA-256 values from
[`install-tools.lock.json`](../tools/install-tools.lock.json). It creates a
loopback-bound Kubernetes API and uses a dedicated kubeconfig in
`~/.local/state/ai-sdlc/<cluster>/kubeconfig`. It does not change the default
kubeconfig/context. `XDG_CACHE_HOME` and `XDG_STATE_HOME` are supported.

## What is installed

| Resource | Purpose |
| --- | --- |
| Five Deployments, one container each | Admission, orchestration, worker with SCC, release decisions, relay |
| Five internal ClusterIP Services | Stable component DNS; no external delivery endpoint |
| PostgreSQL StatefulSet + PVC | Durable state and outbox, retained when the StatefulSet is removed |
| ConfigMaps | Runtime settings, frozen shadow authority policy, contract and pinned ADEV profile |
| Migration Job | Schema setup and component-role reconciliation, using the bootstrap credential |
| ServiceAccount, security contexts, NetworkPolicy | Kubernetes workload boundaries |
| Per-component Secret references | Distinct database passwords and signing identities stay out of Git and Helm values |

The ADEV profile is copied from `scanalesespinoza/adev` revision
`340f705718001ee45067e6220d64ece31fab30cd`,
`implementation/profiles/ai-sdlc.yaml`. Its digest is computed from the
packaged file and passed to the worker. Future policy updates must be
reviewed Git changes; issues cannot replace this bundle.

The local bootstrap database password is randomly generated once and sent to the
Kubernetes API over stdin. Subsequent runs reuse the existing Secret. The
password never appears in command arguments, Git, rendered chart output or
Helm values. Preserve the Secret together with PostgreSQL data; backup and
restore automation remains #442. The Quay publication robot is **not** an
installation credential: the image repository is public, so local pulls
need no publisher token.

## Component identities

### Optional local GitHub client

For an explicitly selected local account, the same installer can import the
credential of an authenticated GitHub CLI session:

```bash
gh auth login --hostname github.com
./install.sh credentials --github-auth gh-cli --provider podman
```

This command requires an existing installer-owned cluster and matching authority
repository. `./install.sh local --github-auth gh-cli` also configures it after
installation. `plan --github-auth gh-cli` reports intent without accessing `gh`
or creating credentials. The default remains `none`; there is no automatic
fallback from a GitHub App to a user credential.

[`gh auth token`](https://cli.github.com/manual/gh_auth_token) retrieves the active
credential; it does not create a PAT or narrow its existing GitHub permissions.
The installer verifies account identity and write access to the configured
repository using bounded read-only GitHub API requests, then sends the credential
to the owned `ai-sdlc-github-client` Secret over stdin. It never prints the token,
places it in command arguments, or stores it in Git or Helm values. Reapplication
revalidates and preserves that Secret. To import the current CLI credential again,
use `credentials --github-auth gh-cli --refresh-github-credential`; replacement
preserves the Secret UID and checks concurrent changes.

The Secret is reserved for trusted GitHub operations. This maintenance release
does not mount it in any workload, including the worker; generated Jobs must
never receive it. Credential validation establishes access only, not a completed
autonomous delivery cycle. GitHub App runtime integration remains tracked in #425.

### Internal component credentials

Chart 0.3 separates the bootstrap database credential from every service.
A bounded migration Job owns schema and role setup. Each service receives only
its own database LOGIN and Ed25519 identity, and refuses startup if the mounted
policy, signing key, database identity or required migration is missing.
Readiness checks those dependencies again after startup.

The installer generates eight distinct database passwords and eight signing
identities through the pinned migrator image. Private helper output is captured
with container logging disabled; Secrets are created over stdin. Only public
verification keys enter Helm values and the authority ConfigMap. No private
keys or passwords are written to Git, Helm release values or command arguments.

Reapplication preserves existing credentials and policy. New Secrets carry the
installer's cluster UID; a conflicting owner, key, scope or credential map
stops setup before those resources are changed. The default scope is
`os-santiago/ai-sdlc` and the test environment `ai-sdlc-production`; use
`--repository` and `--environment` to select another scope on first install.
These flags do not silently rewrite an existing authority policy.

Keep the component Secrets, the migrator credential map, authority ConfigMap
and database together when preparing a backup. Losing a signing Secret requires
restoration or a reviewed key rotation; reinstalling does not manufacture a new
key under an existing trusted public key.

## Git-managed desired state

The source of truth is the chart plus committed
[`local/values.yaml`](../runtime/k8s/deploy/local/values.yaml),
[`local/kind.yaml`](../runtime/k8s/deploy/local/kind.yaml) and
`local/images.lock.json`. Checkout a reviewed revision before installing.
Every runtime image is pinned to its Quay digest; the lock is validated
before any cluster mutation. `plan` validates/renders configuration and
reports intended images without creating a cluster or changing Kubernetes.
It may populate the verified client-tool cache.

Run `./install.sh local` again to reconcile the committed desired state.
The installer waits for kind networking and storage before starting application
setup. Each Helm revision runs a new bounded migration Job. Readiness polling
allows recovery after an earlier infrastructure outage without rotating keys
or deleting the database. `--timeout` bounds each wait operation, not the total
installer duration; an unsuccessful wait still exits nonzero.
Helm owns workload updates, and a config checksum rolls Deployments when
their configuration changes. This first version provides **explicit
reconciliation**, not continuous drift correction. No Flux or Argo CD is
installed. Argo CD can be considered when unattended reconciliation becomes
a requirement; its source would remain this same chart and values.

Persistent state and unrelated cluster resources must survive reapplication.
The installer does not implement automatic database rollback, production
backup or destructive uninstall. Helm history/rollback is available for
compatible manifests; database downgrade safety is not implied (#441).

## Installation readiness versus autonomous delivery

This release installs **maintenance mode**. Helm readiness means the five
services have valid component identities and database permissions, and the migration Job completed. It does not mean an issue was delivered.
HTTP mutation routes return `503 runtime_in_maintenance`; relay delivery and
generative execution are disabled. The chart rejects `runtime.mode=active`
until the issue-only intake and component authority work is implemented.
No model/API/GitHub write credentials are mounted in this mode.

The default kind CNI does not enforce Kubernetes NetworkPolicy. Policies
are rendered for compatible CNIs, but are not proof of isolation on kind.
Maintenance mode and the absence of public component ingress are essential
boundaries here; [NetworkPolicy enforcement requires a supporting CNI](https://kubernetes.io/docs/concepts/services-networking/network-policies/).
Application authentication and worker isolation remain
#421/#422. Keep the local cluster on a trusted host.

The missing issue adapters, independent verification, PR/merge/release and
production promotion remain tracked under #153. The experimental CodexCLI
wrapper is not installed or authenticated by this infrastructure installer.
Do not submit synthetic tasks to internal endpoints to claim E2E success.

## Image construction and promotion

GitHub Actions [`containers.yml`](../.github/workflows/containers.yml) builds
all six component Containerfiles on pull requests, without publishing or
receiving Quay secrets. Pushes to `main` and trusted `build/**` branches,
or manual dispatch on those branches, publish to:

```text
quay.io/os-santiago/ai-sdlc:<component>-<full-git-sha>
```

The initial platform is `linux/amd64`. BuildKit produces SBOM/provenance
attestations and a per-component image identity artifact. Publication uses
repository secrets `QUAY_ROBOT_USERNAME` and `QUAY_ROBOT_TOKEN`, configured
with `gh secret set`; their values must never be written into this chart.
Do not allow untrusted collaborators to push publishing branches or edit
publication workflows.

Publication does not change deployed state. Promote a verified build by
updating `local/images.lock.json` in a reviewed PR with the five service digests and migrator artifact
digest, then reapply the installer. The workflow does not ingest issues,
invoke SCC for work, merge PRs or advance AI-SDLC delivery stages.

## Validation and troubleshooting

```bash
./install.sh tools
python3 -m unittest discover -s tools -p test_install.py -v
AI_SDLC_TEST_HELM=<verified-helm-path> python3 -m unittest discover -s tools -p test_helm.py -v
./install.sh local --provider podman
python3 tools/verify_local_install.py --provider podman
./install.sh status --provider podman
```

`test_helm.py` needs the pinned Python packages in
[`quality-requirements.txt`](../tools/quality-requirements.txt). CI uses Docker
for its kind installation and repeat-apply check. Both paths exercise the
same entrypoint and manifests. The verifier compares all component and database Secret identities
and content, authority policy, PVC/volume identity and applied migration count before and after
reapplication. It also checks the five Deployment digests, readiness and the
worker's actual UID. It never submits a work item or advances delivery.

Observed on 2026-11-14: fresh kind/Podman installation and repeat application
passed on Linux amd64, with five ready services plus PostgreSQL, preserved
credentials/storage/migrations and worker UID 1000. The selected images come
from source revision `5fd38f90dc7a55f5e210c5bf729eae2b96ffc24a`, published by
[Actions run 43646059156](https://github.com/os-santiago/ai-sdlc/actions/runs/43646059156).
[Docker/kind CI run 43646250244](https://github.com/os-santiago/ai-sdlc/actions/runs/43646250244)
also passed fresh installation and repeat application. These are installation
results in maintenance mode, not autonomous delivery or production evidence.

Observed on 2026-11-15: chart 0.3 with source images `b445938` passed local
kind/Podman recovery and reapplication, preserving all 18 Secrets, authority,
PVC and migrations, with five ready services and worker UID 1000. Read-only
database inspection confirmed eight distinct managed LOGINs without elevated
role flags. [Docker/kind run 43657544998](https://github.com/os-santiago/ai-sdlc/actions/runs/43657544998)
passed fresh installation and reapplication. The local test exposed exhausted
host inotify limits and stale Deployment failure conditions; committed host
preflight, dependency waits and bounded Helm readiness polling address those
observed failures. This evidence still concerns maintenance installation only.

The first local trial exposed a non-numeric worker image user rejected by
Kubernetes; this was corrected before the pinned build above. Database/DNS
startup ordering now causes Kubernetes startup retries rather than silently
running without persistence. Initial pod restarts while PostgreSQL becomes
ready are expected. The installer has a finite wait timeout; Kubernetes may
continue retrying pods after the installer exits with failure.

- **Missing tool/runtime:** install Python/Git and Docker or Podman. The
  installer downloads the Kubernetes clients itself.
- **Existing cluster without ownership record:** choose another `--cluster`;
  do not delete someone else's cluster or fabricate ownership metadata.
- **Image digest missing:** finish a successful publication and commit its
  six digests. The installer does not fall back to mutable development tags.
- **ImagePullBackOff:** check registry connectivity and whether the committed
  digest exists. Never put the publishing robot token in the image values.
- **Database startup failure:** inspect PVC/storage and the database Secret;
  components refuse to silently switch to stateless operation.
- **Helm timeout:** inspect the dedicated context's pods/events and rerun
  after resolving the cause. A failed install exits nonzero and retains
  diagnostic state instead of deleting the cluster or data.

Chart 0.2 changes the previous single-pod layout to separate Deployments and
Services. Existing 0.1 adopters must review a rendered diff and preserve
database credentials/PVCs; the new installer is tested as a fresh local
installation, not an automatic migration of existing production clusters.

Chart 0.3 requires its matching image lock, including the migrator digest.
An upgrade from 0.2 retains the bootstrap database Secret and PVC, creates
separate service identities and applies the additive signed-delivery migration.
Do not combine a 0.3 chart with older service images: older binaries do not use
the new identity/readiness boundary. Migration and role provisioning are
installation operations; they do not enqueue issues or advance delivery state.
