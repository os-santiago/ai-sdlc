"""Resources of the installer-owned kind node containers.

A kind node is a container. Its own cgroup limits bound everything that runs
in the cluster: podman starts it with ``pids_limit = 2048`` (containers.conf),
docker with no pid limit. Three concurrent sandboxed verification pods with
service sidecars exhausted 2048 (issue #859), so the local installer sets the
node container's pid limit right after creating the cluster and reconciles it
on every later install. Memory and CPU caps are optional and off by default.

``podman update`` and ``docker update`` change the container's stored
configuration, not only its running cgroup, so the values survive
``podman restart`` and a stop/start of the node (verified with kind 0.33 and
podman 5.8; see docs/operations/kind-node-resources.md).

The pid budget relates the node limit to the bounds that depend on it: every
pod is capped by the kubelet podPidsLimit (offline-runtime/kubelet-pods.json),
and the Tekton CI quotas decide how many CI pods run at once next to the
node's own baseline and one verification run. The committed policy is
runtime/k8s/deploy/local/node-resources.json.
"""
import json
import os
from pathlib import Path
import re

ROOT = Path(__file__).resolve().parents[1]
LOCAL = ROOT / 'runtime/k8s/deploy/local'
POLICY = LOCAL / 'node-resources.json'
KUBELET_PODS = LOCAL / 'offline-runtime/kubelet-pods.json'
MEMORY = re.compile(r'([1-9][0-9]{0,6})([mg])')
CPUS = re.compile(r'(?:[1-9][0-9]{0,2}|0)(?:\.[0-9]{1,2})?')
MIN_MEMORY = 2 * 1024 ** 3


def policy():
    value = json.loads(POLICY.read_text())
    pids, budget = value.get('pidsLimit', {}), value.get('pidBudget', {})
    if (value.get('version') != 1 or set(value) != {'version', 'pidsLimit', 'pidBudget'}
            or set(pids) != {'default', 'hostShare', 'minimum', 'maximum'}
            or set(budget) != {'nodeBaseline', 'concurrentSandboxPods'}
            or any(type(v) is not int or v < 1 for v in list(pids.values()) + list(budget.values()))
            or not pids['minimum'] <= pids['default'] <= pids['maximum']):
        raise ValueError('Unsupported kind node resource policy')
    return value


def sandbox_pod_pids():
    """The kubelet podPidsLimit: the pid bound of every pod on the node."""
    value = json.loads(KUBELET_PODS.read_text())['podPidsLimit']
    if type(value) is not int or value < 1:
        raise ValueError('Invalid kubelet podPidsLimit policy')
    return value


def host_pid_max(path=Path('/proc/sys/kernel/pid_max')):
    return int(path.read_text().strip())


def host_threads_max(path=Path('/proc/sys/kernel/threads-max')):
    return int(path.read_text().strip())


def desired(pids_limit=None, memory=None, cpus=None, pid_max=None, cpu_count=None, threads_max=None):
    """Validated node resources. pids_limit defaults to the policy default,
    reduced to a quarter of the host's pid_max (or threads-max) on hosts with
    few; an explicit value may reach half of either. memory (for example 8g
    or 6144m) and cpus (for example 4 or 2.5) stay unset unless requested.
    pidsLimitExplicit records whether the operator chose the pid limit: a
    default never lowers a larger limit already on the node."""
    limits = policy()['pidsLimit']
    pid_max = host_pid_max() if pid_max is None else pid_max
    threads_max = host_threads_max() if threads_max is None else threads_max
    host = min(pid_max, threads_max)
    ceiling = min(limits['maximum'], host // 2)
    explicit = pids_limit is not None
    if pids_limit is None:
        pids_limit = min(limits['default'], host // limits['hostShare'])
    if type(pids_limit) is not int or not limits['minimum'] <= pids_limit <= ceiling:
        raise ValueError('The node pid limit must be an integer from %d to %d (half of the host pid_max and threads-max)'
                         % (limits['minimum'], ceiling))
    result = {'pidsLimit': pids_limit, 'pidsLimitExplicit': explicit, 'memory': None, 'cpus': None}
    if memory is not None:
        match = MEMORY.fullmatch(str(memory))
        if not match:
            raise ValueError('--node-memory takes a whole number of MiB or GiB, for example 8g or 6144m')
        result['memory'] = int(match.group(1)) * 1024 ** (2 if match.group(2) == 'm' else 3)
        if result['memory'] < MIN_MEMORY:
            raise ValueError('--node-memory must be at least 2g')
    if cpus is not None:
        if not CPUS.fullmatch(str(cpus)) or float(cpus) <= 0:
            raise ValueError('--node-cpus takes a positive number of CPUs with at most two decimals, for example 4 or 2.5')
        cpu_count = os.cpu_count() if cpu_count is None else cpu_count
        if float(cpus) > cpu_count:
            raise ValueError('--node-cpus exceeds the host CPU count (%d)' % cpu_count)
        result['cpus'] = round(float(cpus), 2)
    return result


def pid_budget(limit, sandbox=False, candidate_pods=0, fetch_pods=0):
    """Refuse a node limit that cannot hold its expected pid load.

    The load is the node baseline (system pods, the runtime and its database,
    measured at ~1100 pids at peak), one verification run when sandboxed
    execution is installed (three concurrent pods: baseline, cross and
    candidate), and every Tekton candidate and fetch pod the quotas admit.
    Every pod counts at the kubelet podPidsLimit, the bound that actually
    caps it, so the budget holds even when every pod reaches its bound and
    relies on no measurement of what a pod usually uses.
    """
    budget = policy()['pidBudget']
    pod = sandbox_pod_pids()
    if type(candidate_pods) is not int or type(fetch_pods) is not int or candidate_pods < 0 or fetch_pods < 0:
        raise ValueError('Pod quotas must be non-negative integers')
    sandbox_pods = budget['concurrentSandboxPods'] if sandbox else 0
    required = budget['nodeBaseline'] + (sandbox_pods + candidate_pods + fetch_pods) * pod
    report = {'nodePidsLimit': limit, 'nodeBaseline': budget['nodeBaseline'], 'podPidsLimit': pod,
              'sandboxPods': sandbox_pods, 'candidatePods': candidate_pods, 'fetchPods': fetch_pods,
              'required': required}
    if limit is not None and required > limit:
        raise ValueError('The node pid limit %d cannot hold its expected load of %d pids (baseline %d, plus %d sandboxed, '
                         '%d candidate and %d fetch pods at their %d-pid bound); raise --node-pids-limit or lower the '
                         'Tekton CI pod quotas. The budget also covers the quotas already installed, so lowering both '
                         'the quotas and the limit takes two runs: first the quotas, then the limit'
                         % (limit, required, budget['nodeBaseline'], sandbox_pods, candidate_pods, fetch_pods, pod))
    return report


def _command(run, args, **kwargs):
    return run([str(a) for a in args], capture_output=True, timeout=60, **kwargs)


def inspect(run, provider, node):
    """The node container's stored limits and ownership label."""
    raw = _command(run, [provider, 'inspect', '--format', '{{json .HostConfig}}|{{json .Config.Labels}}', node]).stdout
    host, labels = raw.strip().split('|', 1)
    host, labels = json.loads(host), json.loads(labels) or {}
    pids = host.get('PidsLimit') or 0
    return {'cluster': labels.get('io.x-k8s.kind.cluster'), 'role': labels.get('io.x-k8s.kind.role'),
            'pidsLimit': pids if pids > 0 else None,
            'memory': host.get('Memory') or None,
            'cpus': round(host['NanoCpus'] / 1e9, 2) if host.get('NanoCpus') else None}


def _cgroup(run, provider, node, name):
    return _command(run, [provider, 'exec', node, 'cat', '/sys/fs/cgroup/' + name]).stdout.strip()


HEADROOM = 1.1


def target_limit(current, want):
    """The pid limit a node is reconciled to: the requested value, or without
    --node-pids-limit the default unless the node already has a larger one."""
    if not want.get('pidsLimitExplicit', True) and current is not None and current > want['pidsLimit']:
        return current
    return want['pidsLimit']


def effective_limit(run, provider, cluster, kind, env, want, names=None):
    """The smallest pid limit the nodes will have after reconciliation, which
    is what the pid budget must be checked against."""
    if names is None:
        names = _command(run, [kind, 'get', 'nodes', '--name', cluster], env=env, check=True).stdout.split()
    if not names:
        raise ValueError('The kind cluster has no nodes')
    return min(target_limit(inspect(run, provider, name)['pidsLimit'], want) for name in names)


def apply(run, provider, cluster, node, want):
    """Set the node container's limits, idempotently, and verify them.

    Only a container labelled as a node of this kind cluster is changed. The
    pid limit is reconciled to the desired value, except that a default (no
    --node-pids-limit) never lowers a larger limit already set, like omitted
    memory and CPU caps, which are left as they are. Lowering a pid limit or
    memory cap requires the node's current use plus 10% to fit below it. A
    stored value whose live cgroup differs (drift) is applied again; the pid
    limit is always written, because podman nests the node's cgroup
    (libpod-<id>.scope/container) and the live value read inside the node is
    only the inner level, while an update rewrites both.
    """
    current = inspect(run, provider, node)
    if current['cluster'] != cluster:
        raise ValueError('Kind node container ownership mismatch: ' + node)
    target = target_limit(current['pidsLimit'], want)
    args = []
    live = _cgroup(run, provider, node, 'pids.max')
    changed = current['pidsLimit'] != target or live != str(target)
    if current['pidsLimit'] is None or current['pidsLimit'] > target or live == 'max' or (live.isdigit() and int(live) > target):
        used = int(_cgroup(run, provider, node, 'pids.current'))
        if used * HEADROOM >= target:
            raise ValueError('Node %s runs %d pids; refusing to lower its pid limit to %d without 10%% headroom'
                             % (node, used, target))
    args += ['--pids-limit', str(target)]
    if want.get('memory') and current['memory'] != want['memory']:
        if current['memory'] is None or current['memory'] > want['memory']:
            used = int(_cgroup(run, provider, node, 'memory.current'))
            if used * HEADROOM >= want['memory']:
                raise ValueError('Node %s uses %d bytes of memory; refusing to cap it at %d without 10%% headroom'
                                 % (node, used, want['memory']))
        # A hard cap: memory plus swap equal to memory, so the node cannot swap past it.
        args += ['--memory', str(want['memory']), '--memory-swap', str(want['memory'])]
    if want.get('cpus') and current['cpus'] != want['cpus']:
        args += ['--cpus', '%g' % want['cpus']]
    changed = changed or len(args) > 2
    _command(run, [provider, 'update', *args, node], check=True)
    observed = inspect(run, provider, node)
    cgroup = _cgroup(run, provider, node, 'pids.max')
    if (observed['pidsLimit'] != target or cgroup != str(target)
            or (want.get('memory') and observed['memory'] != want['memory'])
            or (want.get('cpus') and observed['cpus'] != want['cpus'])):
        raise ValueError('The node container %s did not apply its resource limits' % node)
    return {'node': node, 'pidsLimit': observed['pidsLimit'], 'memory': observed['memory'],
            'cpus': observed['cpus'], 'changed': changed}


def validate_options(args):
    """Node resource options apply to kind installations only, before any effect."""
    given = [name for name in ('node_pids_limit', 'node_memory', 'node_cpus') if getattr(args, name, None) is not None]
    if given and (args.command not in ('plan', 'local', 'sandbox') or args.platform != 'kind'):
        raise ValueError('--node-pids-limit, --node-memory and --node-cpus require plan, local or sandbox on the kind platform')
    if args.platform == 'kind' and args.command in ('plan', 'local', 'sandbox'):
        return desired(getattr(args, 'node_pids_limit', None), getattr(args, 'node_memory', None),
                       getattr(args, 'node_cpus', None))
    return None


def require_cgroup_v2(root=Path('/sys/fs/cgroup')):
    """The pid, memory and CPU limits are verified through cgroup v2 files."""
    if not (root / 'cgroup.controllers').exists():
        raise ValueError('kind node resource limits need a cgroup v2 host; this host uses cgroup v1')


def apply_cluster(run, provider, cluster, kind, env, want, cgroup_root=Path('/sys/fs/cgroup')):
    """Apply the node resources to every node of the cluster."""
    require_cgroup_v2(cgroup_root)
    nodes = _command(run, [kind, 'get', 'nodes', '--name', cluster], env=env, check=True).stdout.split()
    if not nodes:
        raise ValueError('The kind cluster has no nodes')
    return [apply(run, provider, cluster, node, want) for node in sorted(nodes)]
