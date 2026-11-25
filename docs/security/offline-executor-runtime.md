# Offline executor runtime: first isolation rollout

Tracking: #459, first hidden stage of #422 and the [Homedir pilot](../enterprise-pilot.md).
This installs and tests a containment prerequisite. It does not enable issue
execution, grant model access or publish candidate changes.

## Install and verify

Use the same entrypoint after creating an owned local installation:

```bash
./install.sh sandbox --config examples/homedir.local-install.json
# Reapplication checks installed binaries/configuration and repeats conformance.
./install.sh sandbox --config examples/homedir.local-install.json
```

Without a config file, use matching `--cluster`, `--provider`, `--repository`
and `--environment` flags. An existing installer-owned cluster and matching
authority are required; this command never creates a cluster or imports a
credential. It supports the committed single-node Linux amd64 kind topology,
with containerd's v2 configuration. Remote, multi-node, rootless and other
architectures require separate validation.

The installer downloads gVisor release `release-20261109.0`, source
`a0e6be6fb5e03d8ebea13592f36a097c7e9a41b9`, from the upstream release. It checks
the archive and each executable against
[`offline-runtime.lock.json`](../../tools/offline-runtime.lock.json), rejects
unexpected members/links and installs files inside the owned kind node under
`/opt/ai-sdlc/gvisor-release-20261109.0`. No host runtime is changed. gVisor is
an Apache-2.0 project; [upstream source and license](https://github.com/google/gvisor/tree/a0e6be6fb5e03d8ebea13592f36a097c7e9a41b9)
remain authoritative. These integrity pins are not a SLSA build attestation.

Committed [runtime manifests](../../runtime/k8s/deploy/local/offline-runtime/)
define a dedicated containerd handler, shim configuration and RuntimeClass
`ai-sdlc-offline`. The installer preserves runc as the default, validates the
new configuration before replacing it and restarts containerd only when
adding the handler. A restart failure restores the prior containerd config.
Conflicting files, foreign ownership and changed installed binaries stop
reapplication instead of silently replacing them. This is a local node
prerequisite for either Helm or Kustomize workloads; neither deployment
surface launches generative Pods yet.

The successful report is written to
`~/.local/state/ai-sdlc/<cluster>/offline-runtime.json`, including the checked
source/configuration identities, RuntimeClass UID and probe Pod UID. It is the
last successful observation, not continuously renewed authorization to execute.

## Sandbox RuntimeClass per platform

Executor, reviewer, verification and repository-tests pods run in one
configured RuntimeClass. AI-SDLC consumes the platform's standard sandbox
class; it installs its own class only on kind. The name is installation
configuration, never request or model content: the chart value
`sandbox.runtimeClassName` (default `ai-sdlc-offline`) and the matching
`placement.RuntimeClass` (execution) and `runtimeClass` (review) fields that
the installer writes only when the name differs from the default. The chart
rejects a mismatch, and the controllers reject a name that is not a DNS-1123
subdomain.

| Platform | RuntimeClass |
|---|---|
| GKE Sandbox | `gvisor` |
| OpenShift sandboxed containers | `kata` |
| AKS Pod Sandboxing | `kata-mshv-vm-isolation` |
| EKS | Kata on bare-metal node groups |
| kind | `ai-sdlc-offline` (installed by the installer) |

The worker and the verifier check, read-only, that the configured class exists
(`get` on that one `runtimeclasses.node.k8s.io` name, granted by a chart
ClusterRole). Until it does they are not ready. Each poll repeats the check, with
the configuration and client it then uses, immediately before it claims work
or creates a pod; expiry, cleanup, rejection and handoff of already approved
work keep running. There is no fallback to running without the sandbox.

### Transient reads, reasons and the readiness grace

Each check names one bounded reason when it fails. Kubernetes did not answer
in time (`timeout`), the connection failed (`transport`), or the API server
answered 429 (`throttled`) or 5xx (`server-error`): these are *unknown*, not
absent. The other reasons are definite observations or refusals: `not-found`
(404), `forbidden` (401 or 403), `no-handler`, `terminating`, `invalid`
(another status, an oversized or malformed body, or another class), `overhead`,
`scheduling` and `configuration` (no client or an invalid name).

- **Retry within one check.** An unknown read is retried twice, after 200 ms
  and 400 ms, each read bounded to 2 s and the whole check to 5 s and the
  caller's deadline. A definite answer is final at once.
- **New work.** A poll claims work only after a successful read in that poll.
  An unknown class refuses the claim like a missing one (poll state
  `sandbox-runtime-class-unknown` instead of `-unavailable`; the stop names the
  reason, for example `sandbox-runtime-class-timeout` or
  `sandbox-runtime-class-not-found`). Unknown is never treated as available.
- **Work in progress.** No in-flight path reads the class again. A running
  executor, verification or review uses the admission its poll observed before
  the claim (`Session.Bind` and the exact pod comparison check it), so a later
  read failure cannot stop it. A test pins the only callers of the read.
- **Readiness.** The readiness gate stays ready through an unknown read only
  while it observed the same class name(s) available within the last 60 s. A
  definite failure, another class or an older observation makes the component
  not ready at once. The grace covers readiness only, never a claim.

The gate logs only changes, with one of these bounded lines:

```text
sandbox runtime class available
sandbox runtime class unavailable: <reason>
sandbox runtime class unknown: <reason>
sandbox runtime class unknown: <reason>; within grace
```

An error the gate cannot name (an unreadable configuration or service account
token) is `unavailable: preflight`. A readiness probe whose deadline ended
before or during the read is `unknown: timeout`.

The chart value `probes.readinessTimeoutSeconds` sets the readiness probes'
`timeoutSeconds` for every runtime component. `/readyz` bounds its own checks
to 3 s, and the kubelet's default of 1 s cut slow reads short. In evaluation
battery 24 (issue #790), every `sandbox runtime class unavailable` line from the
verifier came with a `/readyz` request that ended at exactly 1.000 s. The probe
had canceled the read; no read ever answered that the class was missing.

The default, 0, omits the field, so the kubelet default applies and an
installation recorded before this setting renders byte-identically (the
upgrade tools compare the re-render with the recorded documents). The
installer sets 5 for new candidate and verification installations, and every
upgrade carries the recorded value forward unchanged. To adopt it on an
existing installation, set the value in a deliberate configuration change.
The kustomize base (`runtime/k8s/deploy/kustomize/base`) keeps the kubelet
default; add `timeoutSeconds: 5` to its readiness probes with an overlay patch
if you deploy from it.

RuntimeClass admission adds the class's `overhead.podFixed` as the pod's
`spec.overhead` and merges the class's `scheduling` nodeSelector and
tolerations. The preflight returns exactly those additions and every pod is
compared with its compiled template plus exactly them; any other added or
changed field still fails. Tolerations that subsume each other or the API
defaults are refused (reason `scheduling`), so the merge is an exact union.

Pod overhead counts against each sandbox namespace's ResourceQuota. Set
`sandbox.podOverhead` (whole millicores and Mi) to grow every sandbox quota by
that overhead for each pod it admits. The controllers refuse a class whose
overhead exceeds the declared headroom (reason `overhead`); with no headroom
only a class without overhead is used. On OpenShift the installer reads the class's overhead and sets the
headroom, rounded up.

On OpenShift, select the class with
`./install.sh local --platform openshift --runtime-class kata ...`; the
installer checks it exists before any change. A later reinstall without
`--runtime-class` refuses to switch an installation that uses another class
back to the default; name the class explicitly to keep or change it. On kind
the installer keeps installing and using `ai-sdlc-offline`.

The chart reads the class field (`placement.RuntimeClass`, `runtimeClass`)
case-insensitively, as the runtime's JSON decoding does, and refuses a document
that spells it more than once. In the verifier, an unavailable reviewer class
stops only new reviews and an unavailable verification class stops only new
verifications; the handoff of approved work continues in the same cycle.

Known limitation: the preflight accepts any handler, because the handler
belongs to the platform; choosing a real sandbox class is the installer's
responsibility.

The `network=none` property below belongs to the kind handler. On another
platform the namespaces' deny-all NetworkPolicy, enforced by the platform's
CNI, is the network boundary, and the platform's class provides the kernel
boundary. Evaluate the platform's sandbox before running untrusted
generations on it.

## Why an offline runtime

Standard NetworkPolicy still provides defense in depth, but a one-time
connectivity check cannot guarantee future CNI enforcement. The inspected
kind network controller has fail-open behavior. The chosen boundary is
gVisor's [disabled external networking](https://gvisor.dev/docs/user_guide/networking/):
`network=none` retains only sandbox loopback. The [shim configuration](https://gvisor.dev/docs/user_guide/containerd/configuration/)
uses systrap, disables host Unix sockets and broad flag overrides, and accepts
no forwarded Pod/container annotations. The privileged runtime lives on the
trusted kind node; fixture workloads remain non-root and unprivileged.

Landlock port restrictions alone cannot establish an IP-specific model-broker
allowlist. A bespoke seccomp/Landlock networking layer was therefore not added.
Fullsend's maintained sandbox/capability design remains a useful comparison;
this stage uses maintained gVisor containment while preserving AI-SDLC's
Kubernetes control-plane architecture.

## Conformance and limits

The installer creates a temporary restricted namespace, deliberately without
NetworkPolicy. A native echo server and native client establish connectivity.
An offline Pod using the pinned SCC image must then prove:

- gVisor observed, non-root UID, zero effective capabilities, read-only root
  and a writable bounded workspace;
- no mounted service-account token, control-plane Secret or runtime socket;
- IPv4 and IPv6 loopback HTTP both work and no external interface is present;
- TCP/UDP over IPv4 and IPv4-mapped IPv6 cannot reach the same server that the
  native client can reach before and after the denial checks;
- SCC 0.4.2 starts inside the offline sandbox;
- a separate Pod using a deliberately missing handler fails to create its
  sandbox, with a matching kubelet event, rather than falling back to runc.

Only the fixture's structured results establish network denial; exec failures,
timeouts and unexpected server responses fail verification. Probe namespaces
and the missing-handler RuntimeClass are deleted with UID preconditions.
Native routed IPv6 and remote-cluster behavior are not validated by an
IPv4-mapped test. No model request, repository task, stage transition or release
is manufactured by these fixtures.

RuntimeClass is opt-in, not a policy that forces every Pod to use it. The next
integration must enforce immutable executor Pod templates/admission rules,
reviewed image and policy identities, bounded resources, no Secret/host mounts,
no API token, and no fallback. Generated output is untrusted and needs an
independent verifier. Runtime/node administrators remain in the trusted
computing base; gVisor does not promise protection against their compromise.

An offline SCC process cannot clone repositories, download dependencies or
contact a model directly. A trusted controller must provide a pinned input
workspace/toolchain and a bounded completion transport. A future mailbox or
fixed `pods/exec` helper must bind requests to attempt/Pod UID, enforce size,
quota, expiry and deduplication, and reject symlinks/arbitrary paths. A model
sidecar in the same offline sandbox also has no external network. None of
those transport or worker-integration capabilities is enabled in this stage.
