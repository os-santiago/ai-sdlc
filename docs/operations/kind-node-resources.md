# kind node resources

A kind node is a container on the host, and its own cgroup limits bound
everything that runs in the cluster. The local installer (`./install.sh local`
and `./install.sh sandbox` on the kind platform) sets those limits right after
it creates the cluster and reconciles them on every later run.

## Options

| Option | Default | Effect |
| --- | --- | --- |
| `--node-pids-limit N` | 8192, or a quarter of the host's `kernel.pid_max` when that is smaller | Pid limit of every node container (2,048 to half of the host's `pid_max`). |
| `--node-memory SIZE` | off | Optional memory cap of every node that runs sandboxed pods (the single node, or each worker; a control-plane with workers stays uncapped), for example `12g` or `6144m` (at least `2g`). Memory plus swap is set to the same value, so the node cannot swap past it. |
| `--node-cpus N` | off | Optional CPU cap of every node that runs sandboxed pods (the single node, or each worker), for example `8` or `6.5` (at most the host CPU count). |
| `--tekton-ci-candidate-pods N` | 1 | Tekton CI candidate-code (`build-test`) pods that may run at once (1 to 32). Requires `--tekton-ci`. |
| `--tekton-ci-fetch-pods N` | 1 | Tekton CI fetch pods that may run at once (1 to 32). Requires `--tekton-ci`. |

`./install.sh plan` validates the same options and prints the resulting
`nodeResources` and pid budget without touching a cluster. The report of
`./install.sh local` records the observed values per node under
`nodeResources`.

The pid limit is reconciled to `--node-pids-limit` when given. Without the
option it is raised to the default when lower (or unlimited), and a larger
limit set earlier is kept, like the memory and CPU caps, which are set when
requested and left as they are otherwise. Lowering a pid limit or memory cap
requires the node's current use plus 10% to fit below the new value. The pid
limit is written on every run: podman nests the node in
`libpod-<id>.scope/container`, so the value visible inside the node is only
the inner level, and an update rewrites both. The pid budget is checked
against the smallest limit the nodes will actually have (a kept larger limit
counts) and reported as `pidBudget.nodePidsLimit`. The limits need a cgroup
v2 host; a cgroup v1 host is refused before any change.
`podman update` cannot remove a cap (`--memory 0` and `--cpus 0` leave it in
place), so to lift one either pass a larger value (up to the host's memory or
CPU count) or recreate the cluster.

## Why

With the podman provider, kind creates the node container with podman's
default `pids_limit = 2048` (`containers.conf`); docker sets no limit. Three
concurrent sandboxed verification pods (baseline, cross and candidate), each
with a Maven JVM and two PostgreSQL sidecars, exhausted 2,048 and runsc failed
with `failed to create new OS thread (errno=11)` (issue #859). The kubelet
`podPidsLimit` and the systemd `DefaultTasksMax` bound pods and scopes inside
the node; neither raises the container's own limit.

## Persistence

`podman update` (and `docker update`) changes the container's stored
configuration, not only its running cgroup. Measured with kind 0.33.0 and
podman 5.8.9 on cgroup v2: after `podman update --pids-limit 8192 --memory 6g
--cpus 4` and `podman restart`, `HostConfig` and the node's
`/sys/fs/cgroup/{pids.max,memory.max,cpu.max}` still showed 8192, 6 GiB and
4 CPUs. A kind node is not restarted with the host, so after a host reboot the
next `./install.sh local` reconciles the limits anyway.

## Dependent bounds

The values that depend on the node limit follow it instead of numbers sized
for 2,048:

- **systemd `DefaultTasksMax`** (per container scope; gVisor runs a whole pod
  in the scope of its first container) is `37.5%` of the node pid limit
  (`offline-runtime/systemd-tasks.conf`): 768 at 2,048 and 3,072 at 8,192.
  systemd resolves the share against the current limit, so it moves with a
  later `podman update`. The offline runtime installer refuses a node where
  that share is below the kubelet `podPidsLimit`.
- **kubelet `podPidsLimit`** stays 768 (`offline-runtime/kubelet-pods.json`).
  It is the bound of one pod (a sandboxed Maven build-test peaked at 603), not
  of the node: raising it with the node would let fewer pods exhaust the node.
  A larger node runs more pods, not larger ones.
- **The pid budget** (`runtime/k8s/deploy/local/node-resources.json`): the
  installer refuses a node limit that cannot hold

  ```text
  node baseline (1,100 at peak, measured)
  + one verification run (3 pods x 768) when the profile is candidate or verification
  + Tekton CI candidate pods x 768
  + Tekton CI fetch pods x 768
  ```

  Every pod counts at the `podPidsLimit` that actually bounds it, not at a
  measured typical size. At the default of 8,192 that is 1,100 + 2,304 + 768 +
  768 = 4,940, so the candidate quota can rise to 5 with one fetch pod. At
  podman's 2,048 the verification profile is refused, which is issue #859.

The installer checks this budget against the smallest limit the nodes will
have. When a run lowers a node's limit, it counts the larger of the installed
and the requested quotas, because the installed ones stay until Tekton CI is
reapplied (and forever if `--tekton-ci` is omitted); lowering both the quotas
and the limit therefore takes two runs, quotas first. A run that keeps or
raises the limit counts the requested quotas, or the installed ones when it
omits `--tekton-ci`. `./install.sh sandbox` counts the verification run too.

No single pod can exceed its `podPidsLimit` and no scope its
`DefaultTasksMax`, both well below the node limit, so one runaway pod cannot
exhaust the node, and since the budget counts every admitted pod at that
bound, all of them together cannot either.

## Worker nodes

`./install.sh local --workers N` (0 to 4, default 0) creates the kind cluster
with N worker nodes next to the control-plane. Zero keeps the committed
single-node `kind.yaml` exactly as before. kind cannot add or remove nodes
later: a reinstall keeps the cluster's topology, refuses a different
`--workers`, and re-derives everything below from the nodes it finds.

All nodes share the host kernel, so workers do not add CPU or memory. They add
separate pid limits and cgroups, and they keep heavy sandboxed work away from
the control-plane. Evidence from the single-node cluster: an independent
review failed its 3-second authority fence (database and API reads) while a
three-pod verification with six PostgreSQL sidecars ran on the same node.

**Placement.**

- Workers are created with the label `ai-sdlc.io/node-role=sandbox` and the
  taint `ai-sdlc.io/sandbox=true:NoSchedule`, so ordinary pods stay off them.
- The installer removes kind's control-plane taint, which kind keeps once a
  cluster has workers. The runtime, its database, the API server and the
  network probes stay on the control-plane.
- `./install.sh sandbox` (and `local` with the candidate or verification
  profile) installs the offline runtime on every worker: runsc, the containerd
  handler, the systemd drop-in and the kubelet limits. It labels each prepared
  worker `ai-sdlc.io/offline-runtime=ready`. The control-plane gets none of
  these.
- The `ai-sdlc-offline` RuntimeClass gets `scheduling.nodeSelector` on that
  label plus a toleration for the taint. Every sandboxed pod (executors,
  reviewers, verification runs, Tekton build-test) therefore lands only on
  prepared workers, without chart changes.
- Tekton's default pod template (`config-defaults`) selects the one **CI
  worker** (label `ai-sdlc.io/ci-node=true`) and tolerates the taint. A
  build-test pod mounts both the run's source claim, bound by its checkout
  pod, and the repository's Maven cache. Both are node-local ReadWriteOnce
  volumes, so on different workers build-test could never schedule. Every
  Tekton pod (fetch and the sandboxed build-test) therefore runs on the CI
  worker, which is prepared like every worker. This covers runs from
  `tools/tekton_ci_run.py` and runs the runtime creates. The CI worker is the
  worker that already holds the CI caches, or the first worker when none is
  bound. Caches bound on several nodes are refused. The CI admission policy
  admits exactly this nodeSelector and toleration in a run's template.
- The installer verifies the runtime with the usual conformance probe and also
  runs one sandboxed pod pinned to each worker (`nodeVerification` in the
  report). `local` also runs the network-policy conformance on the workers
  (`workerNetworkPolicyVerification`).

**Measured isolation.** The test cluster had one control-plane and two
workers on a 16-CPU host. Three pods, each running four busy threads with
allocation churn, ran for 150 sequential API reads (configmaps, pods and nodes
through `kubectl proxy`) per placement:

| Load placement | Workers uncapped: p50 / p95 / max (ms) | Workers at `--node-cpus 4`: p50 / p95 / max (ms) |
| --- | --- | --- |
| none (idle) | 3.0 / 4.8 / 10.3 | 3.1 / 4.6 / 9.2 |
| workers (gVisor, through the RuntimeClass) | 4.9 / 8.1 / 11.7 | 3.0 / 5.8 / 8.2 |
| control-plane (what a single node does) | 5.8 / 11.3 / 14.3 | 7.1 / 12.7 / 20.0 |

All nodes share the host CPUs, so uncapped workers still compete with the
control-plane. With CPU caps on the workers, API latency under worker load
stays at the idle level. Use `--node-cpus` (and `--node-memory`) with
`--workers`; the control-plane stays uncapped.

**Images.** Every node pulls the pinned images from their registries by
digest when a pod first needs them. The install path loads no image archive
into a node, so a new worker needs no extra step.

**Node-local volumes.** local-path volumes are ReadWriteOnce and bound to the
node where their first pod ran, through the PersistentVolume's node affinity.

- The Tekton Maven caches and each run's source claim bind to the CI worker,
  and every later pod that mounts them is scheduled there.
- The sealed dependency cache `deps-<key>` keeps the node of its Tekton fill,
  the CI worker. The three concurrent runs of a verification (baseline, cross,
  candidate) share that claim, so all three run on the CI worker too.
- `tools/dependency_cache_install.py seal` refuses a volume bound to a node the
  RuntimeClass cannot schedule onto.
- Use ReadWriteMany storage if runs must spread across workers.

**Pid budget and quotas.** On every `local` and `sandbox` run, each worker's
kubelet `maxPods` is set to what its pid limit holds: `(limit - 400) // 768 -
1` pods at the 768 `podPidsLimit`, plus kindnet, kube-proxy and one slot for
short helper pods. At 8,192 that is 9 pods and `maxPods: 12`. It follows the
limit when the limit changes, and the scheduler enforces it, so even pods that
node-local volumes draw to one worker can never exceed that worker's pid
limit; extra pods wait.

The CI pods and the verification runs that use the sealed caches all land on
the CI worker, so one verification run (3 pods) plus the Tekton CI candidate
and fetch quotas must fit that one worker's capacity. The quotas keep their
defaults of one each and do not grow with the worker count; raise the CI
worker's capacity with `--node-pids-limit` (9 pods at 8,192, 20 at 16,384).
More workers add room for the other sandboxed pods (executors and reviewers),
separate pid and cgroup limits, and isolation of the control-plane, which
keeps its own baseline and runs no sandboxed pod.

**Host services.** Pods reach the host through SNAT to their node's address,
so a host service that allowlists client addresses (the local model service's
`allowedClients`) must allow every node. The installer prints the addresses
(`Kind node addresses to allow ...`) and records them under
`nodeResources.nodeAddresses`. It never edits host configuration.

**Still single-node.** These tools assume `<cluster>-control-plane` and are
not on the `local` or `sandbox` path. Run them only on single-node clusters:

- The Konflux and build paths: `bootstrap_prepare.py`,
  `build_foundation_install.py`, `build_runtime_install.py`,
  `registry_node_install.py` and `verify_konflux_*`. `--workers` is refused
  with the Konflux platform.
- The browser runtime and verifier fixtures: `browser_runtime.py`,
  `browser_platform_install.py` and `verify_browser_*`.
- The disposable verification fixtures `verify_*`, which require a dedicated
  single-node cluster.
