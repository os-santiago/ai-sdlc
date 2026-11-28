# kind node resources

A kind node is a container on the host, and its own cgroup limits bound
everything that runs in the cluster. The local installer (`./install.sh local`
and `./install.sh sandbox` on the kind platform) sets those limits right after
it creates the cluster and reconciles them on every later run.

## Options

| Option | Default | Effect |
| --- | --- | --- |
| `--node-pids-limit N` | 8192, or a quarter of the host's `kernel.pid_max` when that is smaller | Pid limit of every node container (2,048 to half of the host's `pid_max`). |
| `--node-memory SIZE` | off | Optional memory cap of every node container, for example `12g` or `6144m` (at least `2g`). Memory plus swap is set to the same value, so the node cannot swap past it. |
| `--node-cpus N` | off | Optional CPU cap of every node container, for example `8` or `6.5` (at most the host CPU count). |
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

The installer checks this budget against both the quotas already in the
cluster and the requested ones, so it holds before, during and after a
reconciliation, and when a later run omits `--tekton-ci` (which keeps Tekton
and its quotas). `./install.sh sandbox` counts the verification run too.
Because the installed quotas count, lowering both the quotas and the limit
takes two runs: first the quotas, then the limit.

No single pod can exceed its `podPidsLimit` and no scope its
`DefaultTasksMax`, both well below the node limit, so one runaway pod cannot
exhaust the node, and since the budget counts every admitted pod at that
bound, all of them together cannot either.
