"""Optional, pinned offline gVisor runtime inside an installer-owned kind node.

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
import tomllib
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
MANIFESTS = ROOT / 'runtime/k8s/deploy/local/offline-runtime'
PREFIX = '/opt/ai-sdlc/gvisor-release-20261109.0'
SHIM_CONFIG = '/etc/containerd/ai-sdlc-offline.toml'
NODE_CONFIG = '/etc/containerd/config.toml'
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


def install(kubectl, provider, kind, cluster, cluster_uid, cache, image):
    """Bootstrap only the single node created by the committed kind manifest."""
    import offline_probe

    def command(args, **kwargs):
        return subprocess.run([str(v) for v in args], text=True, check=True,
                              capture_output=True, timeout=90, **kwargs).stdout

    env = dict(os.environ, KIND_EXPERIMENTAL_PROVIDER=provider)
    nodes = command([kind, 'get', 'nodes', '--name', cluster], env=env).splitlines()
    if nodes != [cluster + '-control-plane']:
        raise ValueError('Offline runtime supports only the committed single-node kind topology')
    node = nodes[0]
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

    lock = load_lock()
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
    fragment = (MANIFESTS / 'containerd.toml').read_text()
    desired = node_configuration(before, fragment)
    shim = (MANIFESTS / 'runsc.toml').read_text()
    existing_shim = read(SHIM_CONFIG)
    if existing_shim is not None and (marker is None or existing_shim != shim):
        raise ValueError('Conflicting offline shim configuration')
    existing_class = command(kubectl + ['get', 'runtimeclass', HANDLER, '--ignore-not-found', '-o', 'json'])
    if existing_class:
        resource = json.loads(existing_class)
        if (resource['metadata'].get('annotations', {}).get('ai-sdlc.io/cluster-uid') != cluster_uid
                or resource.get('handler') != HANDLER):
            raise ValueError('Refusing to adopt a foreign RuntimeClass')
    if marker is None:
        downloaded = archive(cache, lock)
        with tempfile.TemporaryDirectory(prefix='ai-sdlc-gvisor-') as directory:
            root = Path(directory) / 'runtime'
            root.mkdir()
            extract_verified(downloaded, root, lock)
            (root / 'ai-sdlc-owner.json').write_text(owner)
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
    if existing_shim is None:
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
    runtime_class = json.loads((MANIFESTS / 'runtimeclass.json').read_text())
    runtime_class['metadata']['annotations'] = {'ai-sdlc.io/cluster-uid': cluster_uid}
    if not existing_class:
        command(kubectl + ['create', '-f', '-'], input=json.dumps(runtime_class))
    result = offline_probe.verify(kubectl, image, HANDLER)
    observed_class = json.loads(command(kubectl + ['get', 'runtimeclass', HANDLER, '-o', 'json']))
    return {'runtimeClass': HANDLER, 'version': lock['version'], 'sourceRevision': lock['sourceRevision'],
            'clusterUID': cluster_uid, 'probeImage': image,
            'runtimeClassUID': observed_class['metadata']['uid'],
            'verifiedAt': datetime.datetime.now(datetime.timezone.utc).isoformat(),
            'archiveSHA256': lock['sha256'], 'configurationSHA256': sha256(shim.encode()),
            'handlerConfigurationSHA256': sha256(fragment.encode()),
            'network': 'none', 'verification': result, 'autonomousExecutionEnabled': False}
