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
            or set(budget) != {'nodeBaseline', 'fetchPod', 'concurrentSandboxPods'}
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


def desired(pids_limit=None, memory=None, cpus=None, pid_max=None, cpu_count=None):
    """Validated node resources. pids_limit defaults to the policy default,
    reduced to a quarter of the host's pid_max on hosts with few pids; memory
    (for example 8g or 6144m) and cpus (for example 4 or 2.5) stay unset
    unless requested."""
    limits = policy()['pidsLimit']
    pid_max = host_pid_max() if pid_max is None else pid_max
    ceiling = min(limits['maximum'], pid_max // 2)
    if pids_limit is None:
        pids_limit = min(limits['default'], pid_max // limits['hostShare'])
    if type(pids_limit) is not int or not limits['minimum'] <= pids_limit <= ceiling:
        raise ValueError('The node pid limit must be an integer from %d to %d (half of the host pid_max)'
                         % (limits['minimum'], ceiling))
    result = {'pidsLimit': pids_limit, 'memory': None, 'cpus': None}
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
    candidate), every Tekton candidate pod the quota admits and every fetch
    pod. Each sandboxed or candidate pod counts at the kubelet podPidsLimit,
    which bounds it; fetch pods run trusted inputs and count at their measured
    size. No single pod or systemd scope can exceed its own bound, so a
    runaway pod cannot exhaust the node either way.
    """
    budget = policy()['pidBudget']
    pod = sandbox_pod_pids()
    if type(candidate_pods) is not int or type(fetch_pods) is not int or candidate_pods < 0 or fetch_pods < 0:
        raise ValueError('Pod quotas must be non-negative integers')
    sandbox_pods = budget['concurrentSandboxPods'] if sandbox else 0
    required = budget['nodeBaseline'] + (sandbox_pods + candidate_pods) * pod + fetch_pods * budget['fetchPod']
    report = {'nodePidsLimit': limit, 'nodeBaseline': budget['nodeBaseline'], 'podPidsLimit': pod,
              'sandboxPods': sandbox_pods, 'candidatePods': candidate_pods, 'fetchPods': fetch_pods,
              'fetchPodPids': budget['fetchPod'], 'required': required}
    if limit is not None and required > limit:
        raise ValueError('The node pid limit %d cannot hold its expected load of %d pids (baseline %d, %d sandboxed '
                         'and %d candidate pods at %d, %d fetch pods at %d); raise --node-pids-limit or lower the '
                         'Tekton CI pod quotas' % (limit, required, budget['nodeBaseline'], sandbox_pods,
                                                    candidate_pods, pod, fetch_pods, budget['fetchPod']))
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


def apply(run, provider, cluster, node, want):
    """Set the node container's limits, idempotently, and verify them.

    Only a container labelled as a node of this kind cluster is changed. The
    pid limit is always reconciled to the desired value; it is lowered only
    when the node currently uses fewer pids. Memory and CPU caps are set when
    requested and left as they are otherwise.
    """
    current = inspect(run, provider, node)
    if current['cluster'] != cluster:
        raise ValueError('Kind node container ownership mismatch: ' + node)
    args = []
    if current['pidsLimit'] != want['pidsLimit']:
        if current['pidsLimit'] is None or current['pidsLimit'] > want['pidsLimit']:
            used = int(_cgroup(run, provider, node, 'pids.current'))
            if used >= want['pidsLimit']:
                raise ValueError('Node %s runs %d pids; refusing to lower its pid limit to %d' % (node, used, want['pidsLimit']))
        args += ['--pids-limit', str(want['pidsLimit'])]
    if want.get('memory') and current['memory'] != want['memory']:
        if current['memory'] is None or current['memory'] > want['memory']:
            used = int(_cgroup(run, provider, node, 'memory.current'))
            if used >= want['memory']:
                raise ValueError('Node %s uses %d bytes of memory; refusing to cap it at %d' % (node, used, want['memory']))
        # A hard cap: memory plus swap equal to memory, so the node cannot swap past it.
        args += ['--memory', str(want['memory']), '--memory-swap', str(want['memory'])]
    if want.get('cpus') and current['cpus'] != want['cpus']:
        args += ['--cpus', '%g' % want['cpus']]
    if args:
        _command(run, [provider, 'update', *args, node], check=True)
    observed = inspect(run, provider, node)
    cgroup = _cgroup(run, provider, node, 'pids.max')
    if (observed['pidsLimit'] != want['pidsLimit'] or cgroup != str(want['pidsLimit'])
            or (want.get('memory') and observed['memory'] != want['memory'])
            or (want.get('cpus') and observed['cpus'] != want['cpus'])):
        raise ValueError('The node container %s did not apply its resource limits' % node)
    return {'node': node, 'pidsLimit': observed['pidsLimit'], 'memory': observed['memory'],
            'cpus': observed['cpus'], 'changed': bool(args)}


def validate_options(args):
    """Node resource options apply to kind installations only, before any effect."""
    given = [name for name in ('node_pids_limit', 'node_memory', 'node_cpus') if getattr(args, name, None) is not None]
    if given and (args.command not in ('plan', 'local', 'sandbox') or args.platform != 'kind'):
        raise ValueError('--node-pids-limit, --node-memory and --node-cpus require plan, local or sandbox on the kind platform')
    if args.platform == 'kind' and args.command in ('plan', 'local', 'sandbox'):
        return desired(getattr(args, 'node_pids_limit', None), getattr(args, 'node_memory', None),
                       getattr(args, 'node_cpus', None))
    return None


def apply_cluster(run, provider, cluster, kind, env, want):
    """Apply the node resources to every node of the cluster."""
    nodes = _command(run, [kind, 'get', 'nodes', '--name', cluster], env=env, check=True).stdout.split()
    if not nodes:
        raise ValueError('The kind cluster has no nodes')
    return [apply(run, provider, cluster, node, want) for node in sorted(nodes)]
