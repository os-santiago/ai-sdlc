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
    pids, budget, workers = value.get('pidsLimit', {}), value.get('pidBudget', {}), value.get('workers', {})
    if (value.get('version') != 1 or set(value) != {'version', 'pidsLimit', 'pidBudget', 'workers'}
            or set(pids) != {'default', 'hostShare', 'minimum', 'maximum'}
            or set(budget) != {'nodeBaseline', 'workerBaseline', 'fetchPod', 'concurrentSandboxPods'}
            or set(workers) != {'maximum', 'podSlots'}
            or any(type(v) is not int or v < 1 for v in list(pids.values()) + list(budget.values()) + list(workers.values()))
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


def worker_capacity(limit):
    """Pods one worker may hold at the kubelet podPidsLimit each, with one pod
    of slack for short helper pods (the storage provisioner's)."""
    return (limit - policy()['pidBudget']['workerBaseline']) // sandbox_pod_pids() - 1


def max_pods(limit):
    """The kubelet maxPods of a worker: its capacity plus the per-node
    daemonset pods and the slack slot. The scheduler enforces it, so no
    placement, including pods drawn to one node by a node-local volume, can
    put more pods on a worker than its pid limit holds."""
    return worker_capacity(limit) + policy()['workers']['podSlots']


def pid_budget(limit, sandbox=False, candidate_pods=0, fetch_pods=0, workers=0):
    """Refuse a node limit that cannot hold its expected pid load.

    Single node: the load is the node baseline (system pods, the runtime and
    its database, measured at ~1100 pids at peak), one verification run when
    sandboxed execution is installed (three concurrent pods: baseline, cross
    and candidate), every Tekton candidate pod the quota admits and every
    fetch pod. Each sandboxed or candidate pod counts at the kubelet
    podPidsLimit, which bounds it; fetch pods run trusted inputs and count at
    their measured size. No single pod or systemd scope can exceed its own
    bound, so a runaway pod cannot exhaust the node either way. The fetch
    term is a measurement (146 for a Maven pod with tests), not a limit: only
    the podPidsLimit caps a fetch pod, so the budget assumes fetch pods stay
    near their measured size, which their reviewed, scriptless inputs make
    likely.

    With workers, sandboxed and CI pods run only on the workers, and each
    worker's kubelet maxPods (max_pods) caps it at what its pid limit holds,
    every pod, fetch pods included, counted at its podPidsLimit. The quotas
    then only need to fit the workers' combined capacity, so they scale with
    the worker count; the control-plane keeps its own baseline.
    """
    budget = policy()['pidBudget']
    pod = sandbox_pod_pids()
    if (type(candidate_pods) is not int or type(fetch_pods) is not int or type(workers) is not int
            or candidate_pods < 0 or fetch_pods < 0 or workers < 0):
        raise ValueError('Pod quotas and the worker count must be non-negative integers')
    sandbox_pods = budget['concurrentSandboxPods'] if sandbox else 0
    if workers:
        capacity = worker_capacity(limit)
        required = sandbox_pods + candidate_pods + fetch_pods
        report = {'nodePidsLimit': limit, 'controlPlaneBaseline': budget['nodeBaseline'],
                  'workerBaseline': budget['workerBaseline'], 'podPidsLimit': pod, 'workers': workers,
                  'workerPodCapacity': capacity, 'workerMaxPods': max_pods(limit), 'sandboxPods': sandbox_pods,
                  'candidatePods': candidate_pods, 'fetchPods': fetch_pods, 'requiredPods': required}
        if budget['nodeBaseline'] > limit or capacity < 1 or required > workers * capacity:
            raise ValueError('%d workers with a pid limit of %d hold %d pods (%d each); one verification run and the '
                             'Tekton CI pod quotas need %d; raise --node-pids-limit or --workers or lower the quotas'
                             % (workers, limit, workers * max(capacity, 0), max(capacity, 0), required))
        return report
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


def apply_cluster(run, provider, cluster, kind, env, want, nodes=None):
    """Apply the node resources to every node of the cluster.

    Every node gets the pid limit. The optional memory and CPU caps go to the
    nodes that run sandboxed pods: the single node, or each worker. The
    control-plane of a cluster with workers stays uncapped, so heavy
    sandboxed work cannot starve the API server or the runtime database."""
    if nodes is None:
        names = _command(run, [kind, 'get', 'nodes', '--name', cluster], env=env, check=True).stdout.split()
        if not names:
            raise ValueError('The kind cluster has no nodes')
        nodes = {'controlPlane': None, 'workers': []}
    else:
        names = [nodes['controlPlane']] + nodes['workers']
    uncapped = dict(want, memory=None, cpus=None)
    return [apply(run, provider, cluster, node, uncapped if nodes['workers'] and node == nodes['controlPlane'] else want)
            for node in sorted(names)]


# Multi-node topology (--workers). Workers run every sandboxed and CI pod; the
# control-plane keeps the runtime, its database and the API server.
ROLE_LABEL = 'ai-sdlc.io/node-role'
ROLE_SANDBOX = 'sandbox'
READY_LABEL = 'ai-sdlc.io/offline-runtime'
READY = 'ready'
SANDBOX_TAINT = {'key': 'ai-sdlc.io/sandbox', 'value': 'true', 'effect': 'NoSchedule'}
CONTROL_PLANE_TAINT = 'node-role.kubernetes.io/control-plane'
IMAGE_LINE = re.compile(r'^    image: (\S+)$', re.M)


def validate_workers(workers):
    maximum = policy()['workers']['maximum']
    if workers is not None and (type(workers) is not int or not 0 <= workers <= maximum):
        raise ValueError('--workers must be an integer from 0 to %d' % maximum)
    return workers


def cluster_config(workers):
    """The kind cluster configuration: the committed kind.yaml, plus one
    worker per requested node with the same pinned image, the sandbox role
    label and the sandbox taint. Zero workers is kind.yaml unchanged."""
    base = (LOCAL / 'kind.yaml').read_text()
    if not workers:
        return base
    images = IMAGE_LINE.findall(base)
    if len(images) != 1:
        raise ValueError('kind.yaml must pin exactly one node image')
    worker = ('  - role: worker\n'
              '    image: %s\n'
              '    labels:\n'
              '      %s: %s\n'
              '    kubeadmConfigPatches:\n'
              '      - |\n'
              '        kind: JoinConfiguration\n'
              '        nodeRegistration:\n'
              '          taints:\n'
              '            - key: %s\n'
              '              value: "%s"\n'
              '              effect: %s\n') % (images[0], ROLE_LABEL, ROLE_SANDBOX, SANDBOX_TAINT['key'],
                                              SANDBOX_TAINT['value'], SANDBOX_TAINT['effect'])
    return base.rstrip('\n') + '\n' + worker * workers


def topology(run, provider, kind, cluster, env):
    """The control-plane and workers of an installer-owned kind cluster.

    Only kind's own naming and labels are accepted: one control-plane named
    <cluster>-control-plane and workers named <cluster>-worker[N], every
    container labelled with this cluster and its role."""
    names = sorted(_command(run, [kind, 'get', 'nodes', '--name', cluster], env=env, check=True).stdout.split())
    control, workers = [], []
    for name in names:
        node = inspect(run, provider, name)
        if node['cluster'] != cluster:
            raise ValueError('Kind node container ownership mismatch: ' + name)
        if node['role'] == 'control-plane' and name == cluster + '-control-plane':
            control.append(name)
        elif node['role'] == 'worker' and re.fullmatch(re.escape(cluster) + r'-worker([2-9][0-9]*)?', name):
            workers.append(name)
        else:
            raise ValueError('Unsupported kind node: ' + name)
    if len(control) != 1:
        raise ValueError('The kind cluster must have exactly one control-plane node')
    return {'controlPlane': control[0], 'workers': workers}


def sandbox_nodes(nodes):
    """Nodes that run sandboxed pods: the workers, or the single node."""
    return nodes['workers'] or [nodes['controlPlane']]


def schedule(run, kubectl, nodes):
    """Keep sandboxed and CI pods on the workers and the runtime off them.

    kind keeps the control-plane taint once a cluster has workers; the runtime
    has no toleration for it, so the installer removes it. Workers carry the
    sandbox taint and role label from creation; this re-applies them when
    missing. Nothing changes on a single-node cluster."""
    if not nodes['workers']:
        return
    control = json.loads(_command(run, kubectl + ['get', 'node', nodes['controlPlane'], '-o', 'json']).stdout)
    if any(t.get('key') == CONTROL_PLANE_TAINT for t in control['spec'].get('taints') or []):
        _command(run, kubectl + ['taint', 'node', nodes['controlPlane'], CONTROL_PLANE_TAINT + ':NoSchedule-'])
    for worker in nodes['workers']:
        node = json.loads(_command(run, kubectl + ['get', 'node', worker, '-o', 'json']).stdout)
        if not any(t.get('key') == SANDBOX_TAINT['key'] and t.get('value') == SANDBOX_TAINT['value']
                   and t.get('effect') == SANDBOX_TAINT['effect'] for t in node['spec'].get('taints') or []):
            _command(run, kubectl + ['taint', 'node', worker, '--overwrite',
                                     '%s=%s:%s' % (SANDBOX_TAINT['key'], SANDBOX_TAINT['value'], SANDBOX_TAINT['effect'])])
        if node['metadata'].get('labels', {}).get(ROLE_LABEL) != ROLE_SANDBOX:
            _command(run, kubectl + ['label', 'node', worker, '--overwrite', ROLE_LABEL + '=' + ROLE_SANDBOX])


def toleration():
    return {'key': SANDBOX_TAINT['key'], 'operator': 'Equal', 'value': SANDBOX_TAINT['value'],
            'effect': SANDBOX_TAINT['effect']}


def runtime_class_scheduling():
    """RuntimeClass scheduling: sandboxed pods land only on prepared nodes."""
    return {'nodeSelector': {READY_LABEL: READY}, 'tolerations': [toleration()]}


def tekton_pod_template():
    """Tekton's default pod template on a cluster with workers. Every task
    pod of a run shares the run's node-local source claim, and the Maven cache
    is node-local too, so fetch pods must run where build-test pods (gVisor,
    workers only) can follow them."""
    return {'nodeSelector': {ROLE_LABEL: ROLE_SANDBOX}, 'tolerations': [toleration()]}


def node_addresses(run, provider, names):
    """The node IPs on kind's network. Pods leave the node through SNAT to
    these addresses, so a host service that allowlists clients (the local
    model service) must allow each of them."""
    result = {}
    for name in names:
        raw = _command(run, [provider, 'inspect', '--format', '{{json .NetworkSettings.Networks}}', name]).stdout
        networks = json.loads(raw) or {}
        address = (networks.get('kind') or {}).get('IPAddress')
        if not address:
            raise ValueError('Cannot read the kind network address of ' + name)
        result[name] = address
    return result
