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
            or set(budget) != {'nodeBaseline', 'workerBaseline', 'concurrentSandboxPods'}
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


def pid_budget(limit, sandbox=False, candidate_pods=0, fetch_pods=0, workers=0, shared=False):
    """Refuse a node limit that cannot hold its expected pid load.

    Single node: the load is the node baseline (system pods, the runtime and its database,
    measured at ~1100 pids at peak), one verification run when sandboxed
    execution is installed (three concurrent pods: baseline, cross and
    candidate), and every Tekton candidate and fetch pod the quotas admit.
    Every pod counts at the kubelet podPidsLimit, the bound that actually
    caps it, so the budget holds even when every pod reaches its bound and
    relies on no measurement of what a pod usually uses.

    With workers, every Tekton CI pod runs on one CI worker: a run's source
    claim and the Maven caches are node-local (ReadWriteOnce), so they must
    share a node, and the sealed dependency caches the Tekton fill writes
    keep each verification run there too. Its kubelet maxPods (max_pods)
    caps it at what its pid limit holds, every pod counted at its
    podPidsLimit, and one verification run plus the CI quotas must fit that
    one worker. Other workers run the remaining sandboxed pods (executors,
    reviewers); the control-plane keeps its own baseline.
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
        if shared:
            report['sharedStorage'] = True
            # ReadWriteMany caches: CI and verification pods spread over every
            # worker; each worker's maxPods still caps it at its pid limit.
            # A sealed dependency cache may still be node-local (ReadWriteOnce),
            # which puts one verification run on one worker.
            if (budget['nodeBaseline'] > limit or capacity < max(1, sandbox_pods)
                    or required > workers * capacity):
                raise ValueError('%d workers with a pid limit of %d hold %d pods (%d each); one verification run and '
                                 'the Tekton CI pod quotas (installed or requested) need %d; raise --node-pids-limit '
                                 'or lower the quotas, quotas first when lowering both'
                                 % (workers, limit, workers * max(capacity, 0), max(capacity, 0), required))
            return report
        if budget['nodeBaseline'] > limit or capacity < 1 or required > capacity:
            raise ValueError('The CI worker with a pid limit of %d holds %d pods; one verification run and the Tekton CI '
                             'pod quotas (installed or requested) need %d, all on that worker because their volumes '
                             'are node-local; raise --node-pids-limit or lower the quotas, quotas first when lowering '
                             'both' % (limit, max(capacity, 0), required))
        return report
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


def lowers_limit(run, provider, names, want):
    """Whether reconciling would lower any node's pid limit (an unlimited
    node counts as lowered)."""
    for name in names:
        current = inspect(run, provider, name)['pidsLimit']
        if current is None or target_limit(current, want) < current:
            return True
    return False


def admitted_pods_fit(run, kubectl, node, target):
    """Before lowering a worker's limit: the sandboxed pods already admitted
    there (outside kube-system), each able to grow to the podPidsLimit, plus
    the worker baseline must fit the new limit. A lower maxPods only stops
    new admissions."""
    raw = _command(run, kubectl + ['get', 'pods', '-A', '--field-selector',
                                   'spec.nodeName=' + node + ',status.phase!=Succeeded,status.phase!=Failed',
                                   '-o', 'json']).stdout
    pods = [p for p in (json.loads(raw) if raw.strip() else {}).get('items', [])
            if p['metadata'].get('namespace') != 'kube-system']
    required = policy()['pidBudget']['workerBaseline'] + len(pods) * sandbox_pod_pids()
    if required > target:
        raise ValueError('Worker %s runs %d pods that may grow to %d pids in total; refusing to lower its limit to %d '
                         'until they end' % (node, len(pods), required, target))
    return required


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


def validate_options(args, cgroup_root=Path('/sys/fs/cgroup')):
    """Node resource options apply to kind installations only, before any
    effect; local and sandbox refuse a cgroup v1 host here, before host
    prerequisites or cluster creation."""
    given = [name for name in ('node_pids_limit', 'node_memory', 'node_cpus') if getattr(args, name, None) is not None]
    if given and (args.command not in ('plan', 'local', 'sandbox') or args.platform != 'kind'):
        raise ValueError('--node-pids-limit, --node-memory and --node-cpus require plan, local or sandbox on the kind platform')
    if args.platform == 'kind' and args.command in ('plan', 'local', 'sandbox'):
        if args.command in ('local', 'sandbox'):
            require_cgroup_v2(cgroup_root)
        return desired(getattr(args, 'node_pids_limit', None), getattr(args, 'node_memory', None),
                       getattr(args, 'node_cpus', None))
    return None


def require_cgroup_v2(root=Path('/sys/fs/cgroup')):
    """The pid, memory and CPU limits are verified through cgroup v2 files."""
    if not (root / 'cgroup.controllers').exists():
        raise ValueError('kind node resource limits need a cgroup v2 host; this host uses cgroup v1')


def apply_cluster(run, provider, cluster, kind, env, want, nodes=None, cgroup_root=Path('/sys/fs/cgroup')):
    """Apply the node resources to every node of the cluster.

    Every node gets the pid limit. The optional memory and CPU caps go to the
    nodes that run sandboxed pods: the single node, or each worker. The
    control-plane of a cluster with workers stays uncapped, so heavy
    sandboxed work cannot starve the API server or the runtime database."""
    require_cgroup_v2(cgroup_root)
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
# Shared storage (--shared-storage): one host directory mounted into every
# node, served as a ReadWriteMany StorageClass (tools/shared_storage.py).
SHARED_PATH = '/var/local/ai-sdlc-shared'
SHARED_CLASS = 'ai-sdlc-shared'
CI_LABEL = 'ai-sdlc.io/ci-node'
CI = 'true'
READY = 'ready'
SANDBOX_TAINT = {'key': 'ai-sdlc.io/sandbox', 'value': 'true', 'effect': 'NoSchedule'}
CONTROL_PLANE_TAINT = 'node-role.kubernetes.io/control-plane'
IMAGE_LINE = re.compile(r'^    image: (\S+)$', re.M)


def validate_workers(workers):
    maximum = policy()['workers']['maximum']
    if workers is not None and (type(workers) is not int or not 0 <= workers <= maximum):
        raise ValueError('--workers must be an integer from 0 to %d' % maximum)
    return workers


def cluster_config(workers, shared_dir=None):
    """The kind cluster configuration: the committed kind.yaml, plus one
    worker per requested node with the same pinned image, the sandbox role
    label and the sandbox taint; the first worker is also the CI worker.
    With shared_dir (shared storage), every node mounts that one host
    directory at SHARED_PATH and the CI worker label is not set. Zero
    workers is kind.yaml unchanged."""
    base = (LOCAL / 'kind.yaml').read_text()
    if not workers:
        if shared_dir is not None:
            raise ValueError('Shared storage needs workers')
        return base
    mount = '' if shared_dir is None else (
        '    extraMounts:\n      - hostPath: %s\n        containerPath: %s\n' % (shared_dir, SHARED_PATH))
    if shared_dir is not None:
        if not str(shared_dir).startswith('/') or any(c in str(shared_dir) for c in '\n"\':#'):
            raise ValueError('The shared storage directory must be a plain absolute path')
        control = '  - role: control-plane\n'
        if base.count(control) != 1:
            raise ValueError('kind.yaml must declare exactly one control-plane node')
        base = base.replace(control, control + mount)
    images = IMAGE_LINE.findall(base)
    if len(images) != 1:
        raise ValueError('kind.yaml must pin exactly one node image')
    worker = ('  - role: worker\n'
              + mount.replace('%', '%%') +
              '    image: %s\n'
              '    labels:\n'
              '      %s: %s\n'
              '%s'
              '    kubeadmConfigPatches:\n'
              '      - |\n'
              '        kind: JoinConfiguration\n'
              '        nodeRegistration:\n'
              '          taints:\n'
              '            - key: %s\n'
              '              value: "%s"\n'
              '              effect: %s\n')
    blocks = [worker % (images[0], ROLE_LABEL, ROLE_SANDBOX,
                        '      %s: "%s"\n' % (CI_LABEL, CI) if index == 0 and shared_dir is None else '',
                        SANDBOX_TAINT['key'], SANDBOX_TAINT['value'], SANDBOX_TAINT['effect'])
              for index in range(workers)]
    return base.rstrip('\n') + '\n' + ''.join(blocks)


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


def shared_mounted(run, provider, names):
    """Whether every node mounts the shared storage directory (a cluster
    created with --shared-storage); a partial mount is refused."""
    found = []
    for name in names:
        raw = _command(run, [provider, 'inspect', '--format', '{{json .Mounts}}', name]).stdout
        mounts = json.loads(raw) if raw and raw.strip() else []
        found.append(any((m.get('Destination') or m.get('destination')) == SHARED_PATH for m in mounts or []))
    if any(found) and not all(found):
        raise ValueError('Only some kind nodes mount the shared storage directory')
    return bool(found) and all(found)


def sandbox_nodes(nodes):
    """Nodes that run sandboxed pods: the workers, or the single node."""
    return nodes['workers'] or [nodes['controlPlane']]


def schedule(run, kubectl, nodes, ci=None, shared=False):
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
        labels = node['metadata'].get('labels', {})
        if labels.get(ROLE_LABEL) != ROLE_SANDBOX:
            _command(run, kubectl + ['label', 'node', worker, '--overwrite', ROLE_LABEL + '=' + ROLE_SANDBOX])
        # Exactly one CI worker: Tekton pods share node-local claims. With
        # shared storage there is none.
        if shared:
            if CI_LABEL in labels:
                _command(run, kubectl + ['label', 'node', worker, CI_LABEL + '-'])
        elif worker == (ci or nodes['workers'][0]) and labels.get(CI_LABEL) != CI:
            _command(run, kubectl + ['label', 'node', worker, '--overwrite', CI_LABEL + '=' + CI])
        elif worker != (ci or nodes['workers'][0]) and CI_LABEL in labels:
            _command(run, kubectl + ['label', 'node', worker, CI_LABEL + '-'])


def toleration():
    return {'key': SANDBOX_TAINT['key'], 'operator': 'Equal', 'value': SANDBOX_TAINT['value'],
            'effect': SANDBOX_TAINT['effect']}


def runtime_class_scheduling():
    """RuntimeClass scheduling: sandboxed pods land only on prepared nodes."""
    return {'nodeSelector': {READY_LABEL: READY}, 'tolerations': [toleration()]}


CI_VOLUME_PREFIXES = ('ai-sdlc-ci-maven-cache-', 'deps-')


def ci_worker(run, kubectl, nodes):
    """The worker that runs every Tekton CI pod: the node that already holds
    the CI namespace's node-local caches (Maven caches, dependency work
    claims), else the worker that already carries the CI label, else the
    first worker. Caches bound on several nodes are refused, and so is moving
    the label while a PipelineRun is in progress (its source claim is bound
    on the current CI worker)."""
    if not nodes['workers']:
        return None
    raw = _command(run, kubectl + ['get', 'pv', '-o', 'json']).stdout
    hosts = set()
    for volume in (json.loads(raw) if raw.strip() else {}).get('items', []):
        claim = volume.get('spec', {}).get('claimRef') or {}
        if claim.get('namespace') != 'ai-sdlc-ci' or not str(claim.get('name', '')).startswith(CI_VOLUME_PREFIXES):
            continue
        terms = (((volume['spec'].get('nodeAffinity') or {}).get('required') or {}).get('nodeSelectorTerms') or [])
        for term in terms:
            for expression in term.get('matchExpressions') or []:
                if expression.get('key') == 'kubernetes.io/hostname':
                    hosts.update(expression.get('values') or [])
    if len(hosts) > 1:
        raise ValueError('Tekton CI caches are bound to several nodes (%s); every CI run needs them on one node'
                         % ', '.join(sorted(hosts)))
    if hosts and next(iter(hosts)) not in nodes['workers']:
        raise ValueError('Tekton CI caches are bound to %s, which is not a worker' % next(iter(hosts)))
    labelled = [name.removeprefix('node/') for name in
                _command(run, kubectl + ['get', 'nodes', '-l', CI_LABEL + '=' + CI, '-o', 'name']).stdout.split()]
    current = next((name for name in labelled if name in nodes['workers']), None)
    choice = next(iter(hosts)) if hosts else (current or nodes['workers'][0])
    if current and choice != current:
        result = _command(run, kubectl + ['get', 'pipelineruns.tekton.dev', '-n', 'ai-sdlc-ci', '--ignore-not-found',
                                          '-o', 'json'], check=False)
        runs = result.stdout or ''
        if result.returncode:
            # Only an absent PipelineRun type (Tekton not installed) means no
            # runs; any other failure leaves active runs possible.
            if "doesn't have a resource type" not in (result.stderr or ''):
                raise ValueError('Cannot list Tekton PipelineRuns to check for active CI runs; refusing to move the '
                                 'CI worker from %s to %s' % (current, choice))
            runs = ''
        active = [r['metadata']['name'] for r in (json.loads(runs) if runs.strip() else {}).get('items', [])
                  if not (r.get('status') or {}).get('completionTime')]
        if active:
            raise ValueError('Moving the CI worker from %s to %s while PipelineRuns are in progress (%s) would strand '
                             'their source claims; rerun the installer after they finish'
                             % (current, choice, ', '.join(sorted(active)[:3])))
    return choice


def tekton_pod_template(shared=False):
    """Tekton's default pod template on a cluster with workers: every task
    pod runs on the one CI worker. A run's source claim (bound by its checkout
    pod) and the repository's Maven cache are both node-local ReadWriteOnce
    volumes that build-test mounts together; without one shared node they
    bind on different workers and build-test can never schedule. The CI
    worker is prepared like every worker, so build-test (gVisor) runs there."""
    if shared:
        # ReadWriteMany caches: a run's pods follow its source claim, bound
        # by its checkout pod on any sandbox worker.
        return {'nodeSelector': {ROLE_LABEL: ROLE_SANDBOX}, 'tolerations': [toleration()]}
    return {'nodeSelector': {CI_LABEL: CI}, 'tolerations': [toleration()]}


def node_addresses(run, provider, names, required=True):
    """The node IPs on kind's network. Pods leave the node through SNAT to
    these addresses, so a host service that allowlists clients (the local
    model service) must allow each of them. The network is the one kind
    uses (KIND_EXPERIMENTAL_<PROVIDER>_NETWORK, default kind), or a node's
    only network. A node whose address cannot be read is reported as None
    unless required (a cluster with workers)."""
    import os
    network = os.environ.get('KIND_EXPERIMENTAL_%s_NETWORK' % str(provider).upper(), 'kind')
    result = {}
    for name in names:
        raw = _command(run, [provider, 'inspect', '--format', '{{json .NetworkSettings.Networks}}', name]).stdout
        networks = json.loads(raw or 'null') or {}
        entry = networks.get(network) or (next(iter(networks.values())) if len(networks) == 1 else {})
        address = (entry or {}).get('IPAddress')
        if not address and required:
            raise ValueError('Cannot read the kind network address of ' + name)
        result[name] = address or None
    return result
