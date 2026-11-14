# Install AI-SDLC with Helm

The supported first installation target is **Linux amd64 + local kind**.
The same chart renders Kubernetes resources; installing into an existing
remote cluster is a separate, not yet validated onboarding path (#434).
The installer needs Python 3.11+, Git, Internet access and a working Docker
or Podman installation. Allow at least 4 CPUs, 6 GiB available RAM and
15 GiB free disk for kind, PostgreSQL and image downloads. These are initial
planning allowances, not measured production sizing.

```bash
git clone https://github.com/os-santiago/ai-sdlc.git
cd ai-sdlc
./install.sh plan
./install.sh local
./install.sh status
```

Use `--provider podman` or `--provider docker` to select the container
runtime explicitly. `--cluster ai-sdlc-dev` creates a separate named
installation; the installer refuses to adopt an unrelated existing cluster.
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
| ConfigMaps | Runtime settings, shadow contract and pinned ADEV profile |
| ServiceAccount, security contexts, NetworkPolicy | Kubernetes workload boundaries |
| Existing Secret references | Database/model credentials stay out of Git and Helm values |

The ADEV profile is copied from `scanalesespinoza/adev` revision
`340f705718001ee45067e6220d64ece31fab30cd`,
`implementation/profiles/ai-sdlc.yaml`. Its digest is computed from the
packaged file and passed to the worker. Future policy updates must be
reviewed Git changes; issues cannot replace this bundle.

The local database password is randomly generated once and sent to the
Kubernetes API over stdin. Subsequent runs reuse the existing Secret. The
password never appears in command arguments, Git, rendered chart output or
Helm values. Preserve the Secret together with PostgreSQL data; backup and
restore automation remains #442. The Quay publication robot is **not** an
installation credential: the image repository is public, so local pulls
need no publisher token.

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
services and database have started. It does not mean an issue was delivered.
HTTP mutation routes return `503 runtime_in_maintenance`; relay delivery and
generative execution are disabled. The chart rejects `runtime.mode=active`
until the issue-only intake and component authority work is implemented.
No model/API/GitHub write credentials are mounted in this mode.

The default kind CNI does not enforce Kubernetes NetworkPolicy. Policies
are rendered for compatible CNIs, but are not proof of isolation on kind.
Maintenance mode and the absence of public component ingress are essential
boundaries here; application authentication and worker isolation remain
#421/#422. Keep the local cluster on a trusted host.

The missing issue adapters, independent verification, PR/merge/release and
production promotion remain tracked under #153. The experimental CodexCLI
wrapper is not installed or authenticated by this infrastructure installer.
Do not submit synthetic tasks to internal endpoints to claim E2E success.

## Image construction and promotion

GitHub Actions [`containers.yml`](../.github/workflows/containers.yml) builds
all five component Containerfiles on pull requests, without publishing or
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
updating `local/images.lock.json` in a reviewed PR with the five artifact
digests, then reapply the installer. The workflow does not ingest issues,
invoke SCC for work, merge PRs or advance AI-SDLC delivery stages.

## Validation and troubleshooting

```bash
./install.sh tools
python3 -m unittest discover -s tools -p test_install.py -v
AI_SDLC_TEST_HELM=<verified-helm-path> python3 -m unittest discover -s tools -p test_helm.py -v
./install.sh local --provider podman
./install.sh local --provider podman
./install.sh status --provider podman
```

`test_helm.py` needs the pinned Python packages in
[`quality-requirements.txt`](../tools/quality-requirements.txt). CI uses Docker
for its kind installation and repeat-apply check. Both paths exercise the
same entrypoint and manifests.

- **Missing tool/runtime:** install Python/Git and Docker or Podman. The
  installer downloads the Kubernetes clients itself.
- **Existing cluster without ownership record:** choose another `--cluster`;
  do not delete someone else's cluster or fabricate ownership metadata.
- **Image digest missing:** finish a successful publication and commit its
  five digests. The installer does not fall back to mutable development tags.
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
