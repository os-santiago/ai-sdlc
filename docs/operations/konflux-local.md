# Local Konflux validation environment

The existing installer can provision a separate Konflux environment for supply
chain integration work. This is infrastructure validation: a ready cluster does
not establish a successful application build, valid provenance, a SLSA level,
or autonomous issue-to-production delivery.

## Install and inspect

Use Linux amd64, Python 3.11 or newer, Git, and a working Docker or Podman
installation. Allow at least four CPUs and eight GiB **available** memory;
sixteen GiB is recommended. Other running clusters consume additional capacity.
The installer checks the existing Linux inotify prerequisites without changing
host settings. If needed, use the explicit host command documented in
[installation](../installation.md).

```bash
git clone https://github.com/os-santiago/ai-sdlc.git
cd ai-sdlc
./install.sh supply-chain plan --provider podman
./install.sh supply-chain local --provider podman
./install.sh supply-chain status
```

`plan` is offline and performs no tool bootstrap or cluster mutation. `local`
defaults to a maximum of 1,800 seconds for the invocation; use `--timeout` with
30–1,800 seconds to change it. Each external command and download also has a
smaller bound. A slowly progressing HTTP response cannot extend the download
past its wall-clock deadline. Failed installation retains owned state for
inspection and reapplication.

The default cluster is `ai-sdlc-konflux`. Use `--cluster NAME` consistently to
select another isolated cluster. The installer refuses to adopt an existing
cluster without its own ownership record. It verifies the recorded provider
and `kube-system` UID before applying resources; `auto` preserves the recorded
provider. A missing recorded cluster is an error, not permission to replace its
identity. Choose a new name for a deliberately new installation.

## Desired state and reconciliation

Reviewed desired state lives in Git:

- `runtime/k8s/deploy/konflux/upstream.lock.json`: upstream release, source
  archive, operator artifact and image, node image, and seven rendered dependency
  manifest digests with byte limits, plus the required Deployment inventory.
- `runtime/k8s/deploy/konflux/kind.json`: one pinned Kubernetes node, loopback API
  endpoint and no UI host-port mappings.
- `runtime/k8s/deploy/helm/ai-sdlc-konflux-local/`: operator and Konflux resource
  configuration. The installer verifies and stages the upstream operator YAML
  into the chart; a raw checkout is not a complete standalone chart package.

The reviewed dependency order is cert-manager, monitoring CRDs, trust-manager,
Tekton Operator/configuration, Tekton Chains signing setup and admission policy.
Dependencies are rendered from the pinned source and verified before applying.
After cert-manager Deployments become available, the installer checks current
API admission with a server-side dry run of an Issuer. This exercises webhook
TLS trust without persisting a probe or issuing a certificate. The check is
bounded by 180 seconds and the remaining installation deadline; dependent
resources are not applied until it succeeds. A previous startup Job's success
does not substitute for the current admission check on reapplication.
The operator owns Dex, its certificate issuer and the internal registry. No
legacy Dex bootstrap or GitOps controller is needed.

Helm release `ai-sdlc-konflux`, stored in namespace `ai-sdlc-platform`, first
installs the operator and its CRDs, then enables the Konflux resource. Reapply
the same `local` command to reconcile. An existing Konflux resource remains
enabled throughout reapplication; failed API reads cannot be interpreted as
absence. Helm uses client-side patching so Kubernetes' aggregated ClusterRole
rules are preserved. It does not force ownership conflicts or replace resources.

This installer supplies no default users, GitHub App, external registry
credential or public ingress. The internal development registry is enabled.
The UI has an internal NodePort but kind maps no UI ports onto the host. This
setup is intended for an isolated developer environment, not an externally
exposed production Konflux service.

## Evidence and troubleshooting

State is under `${XDG_STATE_HOME:-$HOME/.local/state}/ai-sdlc/CLUSTER/`:

- `konflux-ownership.json` records the provider and cluster UID.
- `kubeconfig` is restricted to the local user.
- `konflux-installation-report.json` records the last completed installation
  observation, resource generations, replica counts and observed image IDs.

`status` prints a fresh observation and returns nonzero unless the Konflux Ready
condition describes its current generation and every Deployment has observed
its current generation with all desired updated, ready and available replicas.
Every required Deployment must exist and have at least one desired replica.
Deleting resources and scaled-to-zero deployments are not ready. The report's `desiredSourceLockSHA256`
identifies the current checkout's lock; it does not independently attest that
every installed resource matches that desired state.

Inspect failures using the explicit cluster kubeconfig, for example:

```bash
KUBECONFIG="$HOME/.local/state/ai-sdlc/ai-sdlc-konflux/kubeconfig" \
  kubectl get konflux,deployments,pods -A
```

Use your installed `kubectl`, or the verified client path printed by
`./install.sh tools`. Keep the original state for diagnosis; do not remove an
ownership marker to make an unrelated cluster adoptable. Reapply after fixing
capacity or connectivity. The command does not delete a failed cluster.

The pinned source/manifests and operator/node image digests do **not** guarantee
that every transitive operand image uses a digest. The report inventories actual
image IDs, which are observations rather than signed build provenance. Future
integration must validate build output digests, signing identities, attestations
and Conforma policy results before a release can advance. Registry and GitHub
App credentials must use secret references, never committed values.

## Validation

```bash
python3 -m unittest discover -s tools -p test_supply_chain_install.py -v
```

Tests cover corrupt artifacts, unsafe archives, stale readiness, ownership
preservation, ambiguous API reads, and real slow-stream/bootstrap deadlines.
The repository installer workflow runs these tests. A complete Konflux cluster
requires the additional memory budget above and is validated separately from
the smaller default CI installation.

Upstream references: [Konflux getting started](https://konflux-ci.dev/docs/getting-started/)
and [policy evaluations](https://konflux-ci.dev/docs/compliance/policy-evaluations/).

### Network conformance before source builds

Every `supply-chain local` apply now runs the same disposable restricted-Pod
network tests as the core installer: reachable positive controls, egress deny,
selective egress, unrestricted egress recovery, ingress deny and restored
connectivity. All commands share the install deadline. Namespace cleanup uses an
atomic UID precondition; command/exec failures never count as successful denial.
An interrupted process can retain its uniquely named `ai-sdlc-netcheck-*`
namespace; inspect ownership before removal.

The pinned kindnet version includes NetworkPolicy enforcement; no extra CNI is
installed. The installation report records the probe image digest, observation
time and point-in-time result. Read-only `status` deliberately reports network
conformance as `not-tested`: deployment readiness and historical observations do
not prove current connectivity. Neither result proves continuous isolation.
Build fixtures must additionally test their actual API/registry egress policy
before running project code. See [the core network contract](../installation.md).
The [isolated Homedir build fixture](konflux-homedir-fixture.md) exercises Maven,
a separate OCI publisher, Chains signatures and independent identity checks.
Its documented gaps remain separate from infrastructure readiness.
