# OpenShift admission profile (M5, in progress)

This page records what the runtime needs on OpenShift and what has been
validated so far, on a local OpenShift 5.0 cluster (Red Hat OpenShift Local,
CRI-O, OVN-Kubernetes). It is not yet a supported installation path.

## What OpenShift changes

Every ai-sdlc workload runs as the fixed non-root UID and GID 1000, with
`fsGroup: 1000` where it needs a writable volume. OpenShift's default SCC,
`restricted-v2`, rejects these: it assigns a UID and fsGroup from the
namespace's range instead.

The standard `nonroot-v2` SCC accepts them unchanged. Admission then adds
fields that the runtime did not compile. These were measured on a pod admitted
under `nonroot-v2`:

| Added by | Field |
|---|---|
| SCC admission | `openshift.io/scc`, `seccomp.security.alpha.kubernetes.io/pod` and `security.openshift.io/validated-scc-subject-type` annotations |
| SCC admission | `spec.securityContext.seLinuxOptions.level`, the namespace's MCS level (for example `s0:c26,c5`) |
| ServiceAccount admission | one `imagePullSecrets` entry, `default-dockercfg-<5 chars>` |
| Admission, for pods that request memory | a `node.kubernetes.io/memory-pressure` toleration (`Exists`, `NoSchedule`) |
| OVN-Kubernetes and Multus, after scheduling | `k8s.ovn.org/pod-networks` and `k8s.v1.cni.cncf.io/network-status` annotations |

The pod transports compare each pod with its compiled template exactly, so
these additions would stop every execution, verification and review.

## The profile

Set `openshift.enabled: true` in the chart values. Then:

- The chart binds the `system:openshift:scc:nonroot-v2` ClusterRole, which
  grants nothing else, to two kinds of subject:
  - the component ServiceAccounts of the release namespace;
  - the `default` ServiceAccount, which workload pods run as without a token,
    in the executors, verifiers, reviewers and browser namespaces when they
    are enabled.
- The runtime configuration sets `AI_SDLC_PLATFORM=openshift`. The pod
  transports then remove only the additions in the table above, and only in
  their exact formats, before the comparison:
  - the SCC annotation must name `nonroot-v2` or `restricted-v2`;
  - the seccomp annotation must be `runtime/default`;
  - the SELinux options may carry only an MCS level;
  - there may be at most one pull secret, and it must be the default
    ServiceAccount's;
  - the network annotations are accepted only once the pod is scheduled;
  - one memory-pressure toleration, in exactly the admitted form, may be added.
- Container image identity: containerd reports the pinned digest in a
  container's `imageID`. CRI-O reports there the platform manifest it resolved
  from a pinned image index, and the exact pinned reference in `image`. Under
  the profile, the transports also accept that CRI-O form: `image` must equal
  the compiled pin exactly, and `imageID` must be a well-formed digest.

  Any other difference still fails closed, including a pod admitted under a
  privileged SCC.
- The chart's DNS egress rules target the `dns-default` pods in
  `openshift-dns` on port 5353. Network policy matches the pod port behind the
  Service's port 53, so the Kubernetes rule (`kube-system`, port 53) would block
  every name lookup, including the database.

Without the value, nothing changes on other Kubernetes distributions.

## Sandbox runtime

Executor, reviewer and verification pods use the configured sandbox
RuntimeClass, `ai-sdlc-offline` by default
([per-platform classes](../security/offline-executor-runtime.md#sandbox-runtimeclass-per-platform)).
On kind its handler is gVisor (`runsc`) under containerd, with networking
disabled. On OpenShift, select the installed class with `--runtime-class`, for
example `kata` for OpenShift sandboxed containers.

On CRI-O, the same pinned `runsc` release was registered as a runtime handler
and failed in three ways:

1. **SELinux label:** gVisor rejects the SELinux process label that CRI-O adds.
2. **Namespace paths:** gVisor cannot join the UTS and IPC namespaces that
   CRI-O pins (`/var/run/utsns/...`).
3. **Exit status and output:** with both removed by a wrapper, and with infra
   containers enabled, the sandbox starts and the containers run to
   completion. However, conmon receives neither their output nor their exit
   status ("process is still alive but not a direct child").

gVisor under CRI-O is therefore not a working sandbox on OpenShift today. The
supported sandbox runtime there is OpenShift sandboxed containers (Kata). The
next M5 step is to evaluate it as the sandbox class (`--runtime-class kata`),
together with the namespace's deny-all NetworkPolicy.

OpenShift sandboxed containers 1.13 was tried on OpenShift Local 5.0. The
host has nested virtualization (AMD), and the operator installed. However,
the `KataConfig` failed at the MachineConfig step: the node could not install
the `kata-containers` RHCOS extension (`Packages not found: 9pfs, cloud-init,
gvisor-tap-vsock-gvforwarder, hyperv-daemons`). OpenShift Local's trimmed
RHCOS image does not carry the extensions content. Deleting the `KataConfig`
returned the pool to its previous configuration. Evaluating Kata therefore
needs an OpenShift cluster with the full RHCOS extensions, on bare metal or
with nested virtualization.

For a local compatibility evaluation only, the RuntimeClass can point to the
default `crun` handler. That keeps the SCC, Pod Security admission, the
read-only root filesystem, the dropped capabilities and the deny-all
NetworkPolicy, but removes the gVisor kernel boundary. Do not run untrusted
generations that way outside a disposable cluster.

## Install

The administrator prepares the cluster:

- a default StorageClass;
- the sandbox RuntimeClass (see above): its name is passed with
  `--runtime-class NAME`, and defaults to `ai-sdlc-offline`;
- a kubeconfig context allowed to create namespaces, RBAC and workloads.

The installing host still needs Docker or Podman, which generate the
installation's identities locally, and the other local installer
prerequisites.

Then the installer targets that context instead of creating a kind cluster:

```bash
./install.sh local --platform openshift --kubeconfig ~/.kube/config --context admin \
  --cluster my-openshift --repository OWNER/REPO --environment ai-sdlc-production \
  --profile verification --independent-review --publication --merge --review-repair \
  --github-auth gh-cli --codex-auth-file ~/.codex/auth.json \
  --model-upstream upstream.json --project-manifest manifest.json \
  --runtime-class kata
```

On first install the installer records the cluster's UID and the context in
its state directory, and it refuses a different cluster or context for that
`--cluster` name later. Use a `--cluster` name of its own: the installer
refuses a state directory that belongs to a kind or Konflux installation, and
it records nothing for a cluster without the OpenShift security API. Before any change it checks three things: the
OpenShift security API, a default StorageClass, and the selected RuntimeClass
when the profile runs generations. It never installs a node runtime. The
worker and the verifier repeat the RuntimeClass check before they become ready. It renders the
chart with `openshift.enabled=true`.

The model endpoint must be reachable from the cluster's pods at a literal
private address. On OpenShift Local with user-mode networking, pods reach the
host's addresses through the VM's network proxy, and the host sees the
connection as coming from its own address.

## Status

| Item | State |
|---|---|
| Fixed UID 1000 under `nonroot-v2` | Validated on OpenShift Local |
| Exact pod comparison with OpenShift admission (`openshift.enabled`) | Validated on OpenShift Local for executor, verification and review pods (the additions above, plus the memory-pressure toleration and CRI-O image identity) |
| Sandbox runtime | gVisor under CRI-O fails. Kata cannot install on OpenShift Local (no RHCOS extensions). The validated run used `crun`, which gives no kernel sandbox. Kata needs a full OpenShift cluster |
| Outer-loop console rollout (`console_rollout.go`) | Not covered: it compares the production pods without the profile |
| Installer path (`install.sh local --platform openshift`) | Shadow and full verification profiles installed on OpenShift Local |
| End-to-end delivery on OpenShift | One issue went from label to merged pull request in 6.5 minutes on OpenShift Local ([evidence](../evidence/2026-11-22-evaluation-batteries-2-3-openshift.md)) |
