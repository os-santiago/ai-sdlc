"""Optional, pinned offline gVisor runtime inside installer-owned kind nodes.

This never launches business work or changes the default runtime. The node is
trusted infrastructure; untrusted executors must have no node/API credentials.
"""
import hashlib
import datetime
import json
import os
from pathlib import Path
import secrets
import subprocess
import tarfile
import tempfile
import time
import tomllib
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
MANIFESTS = ROOT / 'runtime/k8s/deploy/local/offline-runtime'
PREFIX = '/opt/ai-sdlc/gvisor-release-20261109.0'
SHIM_CONFIG = '/etc/containerd/ai-sdlc-offline.toml'
NODE_CONFIG = '/etc/containerd/config.toml'
TASKS_DROPIN = '/etc/systemd/system.conf.d/ai-sdlc-offline-tasks.conf'
HANDLER = 'ai-sdlc-offline'


def sha256(raw):
    return hashlib.sha256(raw).hexdigest()


def load_lock():
    lock = json.loads((ROOT / 'tools/offline-runtime.lock.json').read_text())
    if (lock['version'] != 'release-20261109.0'
            or lock['url'] != 'https://github.com/google/gvisor/releases/download/release-20261109.0/gvisor-x86_64.tar.bz2'
            or not 0 < lock['size'] < 128 * 1024 * 1024):
        raise ValueError('Unsupported offline runtime lock')
    for name, entry in lock['files'].items():
        if (name.startswith('/') or '..' in Path(name).parts or not name
                or not 0 < entry['size'] < 128 * 1024 * 1024
                or len(entry['sha256']) != 64):
            raise ValueError('Invalid offline runtime binary entry')
    return lock


def extract_verified(archive, destination, lock):
    """Extract only individually verified regular files; never extract links."""
    raw = archive.read_bytes()
    if len(raw) != lock['size'] or sha256(raw) != lock['sha256']:
        raise ValueError('Offline runtime archive integrity check failed')
    seen = set()
    with tarfile.open(archive, mode='r:bz2') as source:
        for member in source:
            if member.isdir() and member.name == 'gvisor-bin':
                continue
            expected = lock['files'].get(member.name)
            if (not member.isfile() or expected is None or member.name in seen
                    or member.size != expected['size']):
                raise ValueError('Unexpected offline runtime archive member')
            data = source.extractfile(member).read(expected['size'] + 1)
            if sha256(data) != expected['sha256'] or len(data) != expected['size']:
                raise ValueError('Offline runtime binary integrity check failed')
            target = destination / member.name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
            target.chmod(0o755)
            seen.add(member.name)
    if seen != set(lock['files']):
        raise ValueError('Offline runtime archive is incomplete')


def archive(cache, lock):
    cache.mkdir(parents=True, exist_ok=True)
    target = cache / (lock['version'] + '.tar.bz2')
    if target.exists() and target.stat().st_size == lock['size'] and sha256(target.read_bytes()) == lock['sha256']:
        return target
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(dir=cache, delete=False) as output:
            temporary = Path(output.name)
            with urllib.request.urlopen(lock['url'], timeout=60) as response:
                raw = response.read(lock['size'] + 1)
            if len(raw) != lock['size'] or sha256(raw) != lock['sha256']:
                raise ValueError('Offline runtime download integrity check failed')
            output.write(raw)
        temporary.replace(target)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)
    return target


def node_configuration(current, fragment):
    """Preserve unrelated settings and reject ambiguous or foreign handlers."""
    parsed = tomllib.loads(current)
    if parsed.get('version') != 2:
        raise ValueError('Offline runtime currently requires the pinned kind v2 containerd configuration')
    if parsed.get('imports', []) != []:
        raise ValueError('Containerd imports can override runtime authority; review them separately')
    containerd = parsed['plugins']['io.containerd.grpc.v1.cri']['containerd']
    if containerd.get('default_runtime_name') != 'runc':
        raise ValueError('Refusing to change a non-default kind runtime configuration')
    begin, end = '# BEGIN AI-SDLC OFFLINE RUNTIME v1', '# END AI-SDLC OFFLINE RUNTIME v1'
    if HANDLER in containerd.get('runtimes', {}):
        expected = tomllib.loads(fragment)['plugins']['io.containerd.grpc.v1.cri']['containerd']['runtimes'][HANDLER]
        if (current.count(begin) != 1 or current.count(end) != 1 or fragment not in current
                or containerd['runtimes'][HANDLER] != expected):
            raise ValueError('Conflicting offline runtime handler; preserve and review its configuration')
        return current
    if begin in current or end in current or SHIM_CONFIG in current or PREFIX in current:
        raise ValueError('Incomplete or foreign offline runtime configuration')
    desired = current.rstrip() + '\n\n' + fragment
    tomllib.loads(desired)
    return desired


DROPIN_MARK = '# AI-SDLC offline runtime:'
KUBELET_CONFIG = '/var/lib/kubelet/config.yaml'
KUBELET_MARK = '# AI-SDLC offline runtime: bounded pids per pod'


def node_pid_limit(read):
    """The kind node container's own pid limit, or None when unlimited."""
    value = (read('/sys/fs/cgroup/pids.max') or '').strip()
    if value == 'max':
        return None
    if not value.isdigit():
        raise ValueError('Cannot read the kind node pid limit')
    return int(value)


def system_tasks_max(read, limit):
    """The base systemd scales DefaultTasksMax against: the smallest of the
    node's pid limit, kernel.threads-max and kernel.pid_max - 1."""
    values = [limit] if limit is not None else []
    for path, offset in (('/proc/sys/kernel/threads-max', 0), ('/proc/sys/kernel/pid_max', 1)):
        raw = (read(path) or '').strip()
        if raw.isdigit() and int(raw) > offset:
            values.append(int(raw) - offset)
    return min(values) if values else None


def tasks_share(desired):
    """The DefaultTasksMax share of the reviewed drop-in, in permyriad."""
    value = next(line.split('=', 1)[1].strip() for line in desired.splitlines() if line.startswith('DefaultTasksMax='))
    whole, _, fraction = value.removesuffix('%').partition('.')
    if not value.endswith('%') or not whole.isdigit() or len(fraction) > 2 or (fraction and not fraction.isdigit()):
        raise ValueError('The reviewed task limit must be a percentage of the node pid limit')
    share = int(whole) * 100 + int(fraction.ljust(2, '0') or 0)
    if not 0 < share < 5000:
        raise ValueError('The reviewed task limit must stay below half of the node pid limit')
    return share


def sandbox_task_limit(read, write, command, node_exec, minimum=None):
    """Make the node's per-scope task limit fit sandboxed pods, idempotently.

    gVisor runs every container of a pod (the sandbox, gofers and systrap
    stubs) inside the scope of the pod's first container, and systemd caps
    each container scope at DefaultTasksMax, 15% of the node's pid limit
    (307 for a kind node under podman's default of 2048). A Tekton task pod
    with three or more containers exceeded it and the sandbox died with
    "failed to create new OS thread". The reviewed drop-in sets a share of the
    node's pid limit (37.5%), so the value follows the node container's limit
    (tools/kind_nodes.py) instead of a number sized for one node size: 768 at
    2048, 3072 at 8192. systemd resolves the share against the current limit,
    so a later `podman update --pids-limit` moves it without a re-exec. The
    resolved value (systemd scales the smallest of the pid limit,
    kernel.threads-max and kernel.pid_max - 1) must be at least the kubelet
    podPidsLimit (minimum), so a
    sandboxed pod is bounded by its pod limit; a smaller node is refused. A
    node without a pid limit (docker without one) already has a larger
    default and gets no drop-in. An earlier ai-sdlc drop-in is replaced
    (upgrade or change); a foreign one is refused. It applies to every
    container scope created afterwards, sandboxed or not; every pod is also
    bounded by the kubelet podPidsLimit (pod_pid_limit).
    """
    desired = (MANIFESTS / 'systemd-tasks.conf').read_text()
    share = tasks_share(desired)
    if minimum is None:
        minimum = json.loads((MANIFESTS / 'kubelet-pods.json').read_text())['podPidsLimit']
    limit = node_pid_limit(read)
    base = system_tasks_max(read, limit)
    if limit is not None and base * share // 10000 < minimum:
        raise ValueError('The kind node pid limit is too small for sandboxed pods; raise --node-pids-limit')
    current = read(TASKS_DROPIN)
    if current is not None and not current.startswith(DROPIN_MARK):
        raise ValueError('Conflicting systemd task limit drop-in on the kind node')

    def shown():
        value = command(node_exec + ['systemctl', 'show', '-p', 'DefaultTasksMax', '--value']).strip()
        return None if value == 'infinity' else int(value)
    observed = shown()
    if current is None and limit is None and (observed is None or observed >= minimum):
        return observed
    # systemd floors the share of the smallest of pids.max, threads-max and
    # pid_max - 1.
    expected = None if limit is None else base * share // 10000
    if current != desired:
        command(node_exec + ['mkdir', '-p', '/etc/systemd/system.conf.d'])
        write(TASKS_DROPIN, desired)
    if current != desired or (expected is not None and observed != expected):
        # Re-executing the manager applies the default to scopes created
        # afterwards; running containers keep their limits.
        command(node_exec + ['systemctl', 'daemon-reexec'])
        observed = shown()
    if observed is None or observed < minimum or (expected is not None and observed != expected):
        raise ValueError('The kind node did not apply the sandbox task limit')
    return observed


def pod_pid_limit(read, write, command, node_exec, configz):
    """Bound every pod's pids with the kubelet podPidsLimit (kubelet_settings)."""
    policy = json.loads((MANIFESTS / 'kubelet-pods.json').read_text())
    return kubelet_settings(read, write, command, node_exec, lambda: {'podPidsLimit': configz()},
                            {'podPidsLimit': policy['podPidsLimit']},
                            {'podPidsLimit': set(policy.get('previous', []))})['podPidsLimit']


def kubelet_settings(read, write, command, node_exec, configz, desired, previous=None):
    """Set reviewed kubelet configuration values, idempotently and atomically.

    desired maps a KubeletConfiguration field (podPidsLimit on every node,
    maxPods on workers) to its integer value. New clusters get podPidsLimit
    from deploy/local/kind.yaml. On an existing node the installer stages the
    kubelet configuration with the reviewed values, checks the staged file is
    exactly the original plus those values, moves it into place, restarts the
    kubelet and confirms through the node's /configz that the values are in
    effect; on any failure it restores the original file and restarts the
    kubelet again. A value ai-sdlc set before (marked, or an earlier reviewed
    value listed in previous) is upgraded; any other value is refused. Pods
    created before the change keep running without the bound until they are
    recreated.
    """
    previous = previous or {}
    config = read(KUBELET_CONFIG)
    if config is None:
        raise ValueError('Kubelet configuration is unavailable on the kind node')
    lines = config.splitlines()
    wanted = [key + ': ' + str(value) for key, value in sorted(desired.items())]
    values = {key: [line for line in lines if line.startswith(key + ':')] for key in desired}

    def applied():
        reported = configz()
        return all(reported.get(key) == value for key, value in desired.items())
    if all(values[key] == [key + ': ' + str(value)] for key, value in desired.items()):
        if not applied():
            # The file holds the values but the running kubelet does not (for
            # example an earlier run stopped after the move, before the
            # restart): restart it and check again.
            command(node_exec + ['systemctl', 'restart', 'kubelet'])
            if not applied():
                raise ValueError('The kubelet does not report the configured ' + ', '.join(sorted(desired)) + ' after a restart')
        return dict(desired)
    for key, found in values.items():
        ours = KUBELET_MARK in lines or (len(found) == 1 and found[0].split(':', 1)[1].strip().isdigit()
                                         and int(found[0].split(':', 1)[1]) in previous.get(key, set()))
        if found and found != [key + ': ' + str(desired[key])] and not ours:
            raise ValueError('The kind node kubelet already sets a different ' + key)
    kept = [line for line in lines if line != KUBELET_MARK and not any(line.startswith(key + ':') for key in desired)]
    desired_text = '\n'.join(kept + [KUBELET_MARK] + wanted) + '\n'
    stage = KUBELET_CONFIG + '.ai-sdlc-' + secrets.token_hex(6)
    write(stage, desired_text)
    try:
        staged = read(stage)
        if staged != desired_text or [line for line in staged.splitlines() if line not in [KUBELET_MARK] + wanted] != kept:
            raise ValueError('Staged kubelet configuration differs from the reviewed change')
        if read(KUBELET_CONFIG) != config:
            raise ValueError('Kubelet configuration changed concurrently; no replacement applied')
        command(node_exec + ['mv', '-T', stage, KUBELET_CONFIG])
    finally:
        command(node_exec + ['rm', '-f', '--', stage])
    try:
        command(node_exec + ['systemctl', 'restart', 'kubelet'])
        if not applied():
            raise ValueError('The kubelet does not report the configured values')
    except (ValueError, subprocess.SubprocessError, OSError):
        write(stage, config)
        command(node_exec + ['mv', '-T', stage, KUBELET_CONFIG])
        command(node_exec + ['systemctl', 'restart', 'kubelet'])
        raise ValueError('The kubelet did not apply ' + ', '.join(sorted(desired)) + '; its prior configuration was restored')
    return dict(desired)


def kubelet_config(kubectl, node, command, attempts=30, sleep=time.sleep):
    """The running kubelet's configuration as it reports it through /configz."""
    for _ in range(attempts):
        try:
            raw = command(kubectl + ['get', '--raw', '/api/v1/nodes/' + node + '/proxy/configz'])
            return json.loads(raw).get('kubeletconfig', {})
        except (subprocess.SubprocessError, ValueError, OSError):
            sleep(2)
    raise ValueError('The kubelet /configz did not answer after the restart')


def kubelet_configz(kubectl, node, command, attempts=30, sleep=time.sleep):
    """podPidsLimit as the running kubelet reports it through /configz."""
    return kubelet_config(kubectl, node, command, attempts, sleep).get('podPidsLimit')


def runtime_class(cluster_uid, workers):
    """The reviewed RuntimeClass; with workers it schedules sandboxed pods
    only onto nodes this installer prepared, tolerating their taint."""
    import kind_nodes
    value = json.loads((MANIFESTS / 'runtimeclass.json').read_text())
    value['metadata']['annotations'] = {'ai-sdlc.io/cluster-uid': cluster_uid}
    if workers:
        value['scheduling'] = kind_nodes.runtime_class_scheduling()
    return value


def install(kubectl, provider, kind, cluster, cluster_uid, cache, image):
    """Bootstrap every node that runs sandboxed pods: the single node of the
    committed kind manifest, or each worker of a cluster created with
    --workers (the control-plane then runs no sandboxed pod)."""
    import kind_nodes
    import offline_probe

    def command(args, **kwargs):
        return subprocess.run([str(v) for v in args], text=True, check=True,
                              capture_output=True, timeout=90, **kwargs).stdout

    def run(args, **kwargs):
        return subprocess.run([str(v) for v in args], text=True, check=kwargs.pop('check', True), **kwargs)

    env = dict(os.environ, KIND_EXPERIMENTAL_PROVIDER=provider)
    topology = kind_nodes.topology(run, provider, kind, cluster, env)
    targets = kind_nodes.sandbox_nodes(topology)
    lock = load_lock()
    fragment = (MANIFESTS / 'containerd.toml').read_text()
    shim = (MANIFESTS / 'runsc.toml').read_text()
    # Every node is checked before any node or the API is changed.
    plans = [prepare_node(provider, node, cluster, cluster_uid, lock, fragment, shim, command) for node in targets]
    desired_class = runtime_class(cluster_uid, topology['workers'])
    existing_class = command(kubectl + ['get', 'runtimeclass', HANDLER, '--ignore-not-found', '-o', 'json'])
    if existing_class:
        resource = json.loads(existing_class)
        if (resource['metadata'].get('annotations', {}).get('ai-sdlc.io/cluster-uid') != cluster_uid
                or resource.get('handler') != HANDLER):
            raise ValueError('Refusing to adopt a foreign RuntimeClass')
        if resource.get('scheduling') != desired_class.get('scheduling'):
            raise ValueError('The existing RuntimeClass schedules differently from this cluster topology')
    staged = None
    try:
        if any(plan['marker'] is None for plan in plans):
            downloaded = archive(cache, lock)
            staged = tempfile.TemporaryDirectory(prefix='ai-sdlc-gvisor-')
            root = Path(staged.name) / 'runtime'
            root.mkdir()
            extract_verified(downloaded, root, lock)
            (root / 'ai-sdlc-owner.json').write_text(plans[0]['owner'])
        nodes = {}
        for plan in plans:
            nodes[plan['node']] = install_node(kubectl, provider, plan, lock, fragment, shim, command,
                                               None if staged is None else Path(staged.name) / 'runtime',
                                               worker=bool(topology['workers']))
    finally:
        if staged is not None:
            staged.cleanup()
    for node in targets:
        command(kubectl + ['label', 'node', node, '--overwrite', kind_nodes.READY_LABEL + '=' + kind_nodes.READY])
    if not existing_class:
        command(kubectl + ['create', '-f', '-'], input=json.dumps(desired_class))
    result = offline_probe.verify(kubectl, image, HANDLER)
    per_node = offline_probe.verify_nodes(kubectl, image, HANDLER, targets) if topology['workers'] else None
    observed_class = json.loads(command(kubectl + ['get', 'runtimeclass', HANDLER, '-o', 'json']))
    first = nodes[targets[0]]
    return {'runtimeClass': HANDLER, 'version': lock['version'], 'sourceRevision': lock['sourceRevision'],
            'clusterUID': cluster_uid, 'probeImage': image,
            'runtimeClassUID': observed_class['metadata']['uid'],
            'verifiedAt': datetime.datetime.now(datetime.timezone.utc).isoformat(),
            'archiveSHA256': lock['sha256'], 'configurationSHA256': sha256(shim.encode()),
            'handlerConfigurationSHA256': sha256(fragment.encode()),
            # The smallest node value; None only when every node is unlimited.
            'network': 'none', 'scopeTasksMax': min((n['scopeTasksMax'] for n in nodes.values() if n['scopeTasksMax'] is not None), default=None),
            'podPidsLimit': first['podPidsLimit'], 'verification': result,
            **({'nodes': nodes, 'scheduling': observed_class.get('scheduling'), 'nodeVerification': per_node}
               if topology['workers'] else {}),
            'autonomousExecutionEnabled': False}


def prepare_node(provider, node, cluster, cluster_uid, lock, fragment, shim, command):
    """Read-only preflight of one node; refuses foreign or conflicting state."""
    label = command([provider, 'inspect', '--format', '{{index .Config.Labels "io.x-k8s.kind.cluster"}}', node]).strip()
    if label != cluster:
        raise ValueError('Kind node container ownership mismatch')
    node_exec = [provider, 'exec', node]

    def read(path):
        exists = subprocess.run(node_exec + ['test', '-e', path], timeout=15)
        if exists.returncode == 1:
            return None
        if exists.returncode:
            raise ValueError('Cannot inspect kind node filesystem')
        return command(node_exec + ['cat', path])

    def write(path, value):
        command([provider, 'exec', '-i', node, 'tee', path], input=value)

    owner = json.dumps({'clusterUID': cluster_uid, 'archiveSHA256': lock['sha256']}, sort_keys=True) + '\n'
    marker = read(PREFIX + '/ai-sdlc-owner.json')
    if marker is not None and marker != owner:
        raise ValueError('Offline runtime installation belongs to a different cluster or revision')
    directory_exists = subprocess.run(node_exec + ['test', '-e', PREFIX], timeout=15).returncode
    if directory_exists not in (0, 1) or (directory_exists == 0 and marker is None):
        raise ValueError('Refusing to adopt unowned offline runtime files')
    before = read(NODE_CONFIG)
    if before is None:
        raise ValueError('Kind containerd configuration is unavailable')
    desired = node_configuration(before, fragment)
    existing_shim = read(SHIM_CONFIG)
    if existing_shim is not None and (marker is None or existing_shim != shim):
        raise ValueError('Conflicting offline shim configuration')
    return {'node': node, 'exec': node_exec, 'read': read, 'write': write, 'owner': owner, 'marker': marker,
            'before': before, 'desired': desired, 'existingShim': existing_shim}


def install_node(kubectl, provider, plan, lock, fragment, shim, command, root, worker=False):
    """Install the verified runtime and node limits on one prepared node."""
    import kind_nodes
    node, node_exec, read, write = plan['node'], plan['exec'], plan['read'], plan['write']
    before, desired = plan['before'], plan['desired']
    if plan['marker'] is None:
        command(node_exec + ['mkdir', '-p', '/opt/ai-sdlc'])
        stage = PREFIX + '-stage-' + secrets.token_hex(6)
        try:
            command([provider, 'cp', str(root), node + ':' + stage])
            command(node_exec + ['mv', '-T', stage, PREFIX])
        finally:
            command(node_exec + ['rm', '-rf', '--', stage])
    # Reapplication verifies installed bytes instead of trusting its marker.
    for name, entry in lock['files'].items():
        found = command(node_exec + ['sha256sum', PREFIX + '/' + name]).split()[0]
        if found != entry['sha256']:
            raise ValueError('Installed offline runtime binary differs from its lock')
    if plan['existingShim'] is None:
        write(SHIM_CONFIG, shim)
    if desired != before:
        stage = NODE_CONFIG + '.ai-sdlc-' + secrets.token_hex(6)
        try:
            write(stage, desired)
            command(node_exec + ['containerd', '--config', stage, 'config', 'dump'])
            if read(NODE_CONFIG) != before:
                raise ValueError('Containerd configuration changed concurrently; no replacement applied')
            command(node_exec + ['mv', '-T', stage, NODE_CONFIG])
            try:
                command(node_exec + ['systemctl', 'restart', 'containerd'])
            except (subprocess.SubprocessError, OSError):
                write(stage, before)
                command(node_exec + ['mv', '-T', stage, NODE_CONFIG])
                command(node_exec + ['systemctl', 'restart', 'containerd'])
                raise ValueError('Containerd restart failed; its prior configuration was restored')
        finally:
            command(node_exec + ['rm', '-f', '--', stage])
    command(kubectl + ['wait', '--for=condition=Ready', 'node', node, '--timeout=90s'])
    policy = json.loads((MANIFESTS / 'kubelet-pods.json').read_text())
    settings = {'podPidsLimit': policy['podPidsLimit']}
    limit = node_pid_limit(read)
    if worker:
        if limit is None:
            raise ValueError('A worker node needs a pid limit; re-run the installer to set it')
        # The scheduler then never places more pods on a worker than its pid
        # limit holds, wherever node-local volumes draw them.
        settings['maxPods'] = kind_nodes.max_pods(limit)
    applied = kubelet_settings(read, write, command, node_exec, lambda: kubelet_config(kubectl, node, command),
                               settings, {'podPidsLimit': set(policy.get('previous', []))})
    command(kubectl + ['wait', '--for=condition=Ready', 'node', node, '--timeout=90s'])
    tasks_max = sandbox_task_limit(read, write, command, node_exec)
    return {'scopeTasksMax': tasks_max, 'podPidsLimit': applied['podPidsLimit'], 'nodePidsLimit': limit,
            **({'maxPods': applied['maxPods']} if worker else {})}


def reconcile_max_pods(kubectl, provider, cluster, node, limit):
    """Set a worker's kubelet maxPods from its pid limit on every install, so
    the scheduler's per-node pod cap follows the limit even before (or
    without) ./install.sh sandbox, and after the limit changes."""
    import kind_nodes

    def command(args, **kwargs):
        return subprocess.run([str(v) for v in args], text=True, check=True,
                              capture_output=True, timeout=90, **kwargs).stdout
    label = command([provider, 'inspect', '--format', '{{index .Config.Labels "io.x-k8s.kind.cluster"}}', node]).strip()
    if label != cluster:
        raise ValueError('Kind node container ownership mismatch')
    node_exec = [provider, 'exec', node]

    def read(path):
        exists = subprocess.run(node_exec + ['test', '-e', path], timeout=15)
        if exists.returncode == 1:
            return None
        if exists.returncode:
            raise ValueError('Cannot inspect kind node filesystem')
        return command(node_exec + ['cat', path])

    def write(path, value):
        command([provider, 'exec', '-i', node, 'tee', path], input=value)
    desired = kind_nodes.max_pods(limit)
    kubelet_settings(read, write, command, node_exec, lambda: kubelet_config(kubectl, node, command),
                     {'maxPods': desired})
    command(kubectl + ['wait', '--for=condition=Ready', 'node', node, '--timeout=90s'])
    return desired
