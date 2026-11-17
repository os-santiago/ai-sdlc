"""Separate gVisor browser handler; never changes the offline executor runtime."""
import json
from pathlib import Path
import secrets
import subprocess
import tomllib

import offline_runtime

ROOT = Path(__file__).resolve().parents[1]
MANIFESTS = ROOT / 'runtime/k8s/deploy/local/browser-runtime'
HANDLER = 'ai-sdlc-browser'
SHIM_CONFIG = '/etc/containerd/ai-sdlc-browser.toml'


def node_configuration(current, fragment):
    # Existing offline handler must already be exact and complete. This also
    # checks default runc, no imported overrides and supported config version.
    offline = (offline_runtime.MANIFESTS/'containerd.toml').read_text()
    if offline_runtime.node_configuration(current, offline) != current:
        raise ValueError('Original offline runtime must be installed first')
    parsed = tomllib.loads(current)
    runtimes = parsed['plugins']['io.containerd.grpc.v1.cri']['containerd']['runtimes']
    begin, end = '# BEGIN AI-SDLC BROWSER RUNTIME v1', '# END AI-SDLC BROWSER RUNTIME v1'
    if HANDLER in runtimes:
        expected = tomllib.loads(fragment)['plugins']['io.containerd.grpc.v1.cri']['containerd']['runtimes'][HANDLER]
        if current.count(begin) != 1 or current.count(end) != 1 or fragment not in current or runtimes[HANDLER] != expected:
            raise ValueError('Conflicting browser runtime handler')
        return current
    if begin in current or end in current or SHIM_CONFIG in current:
        raise ValueError('Partial or foreign browser runtime configuration')
    desired = current.rstrip()+'\n\n'+fragment
    actual = tomllib.loads(desired)
    del actual['plugins']['io.containerd.grpc.v1.cri']['containerd']['runtimes'][HANDLER]
    if actual != parsed:
        raise ValueError('Browser handler changes unrelated containerd settings')
    return desired


def install(kubectl, provider, cluster, cluster_uid):
    """Infrastructure operation in an owned single-node kind installation."""
    if provider not in ('podman', 'docker'):
        raise ValueError('Unsupported container provider')
    def command(args, value=None, timeout=30):
        return subprocess.run([str(x) for x in args], input=value, text=True, capture_output=True,
                              timeout=timeout, check=True).stdout
    observed = json.loads(command(kubectl+['get', 'namespace', 'kube-system', '-o', 'json']))
    if observed['metadata']['uid'] != cluster_uid:
        raise ValueError('Cluster ownership changed')
    node = cluster+'-control-plane'
    metadata = json.loads(command([provider, 'inspect', node]))[0]
    if metadata.get('Config', {}).get('Labels', {}).get('io.x-k8s.kind.cluster') != cluster:
        raise ValueError('Node ownership differs')
    cid = metadata['Id']
    def run(args, value=None, timeout=30):
        fresh = json.loads(command([provider, 'inspect', node]))[0]
        if fresh['Id'] != cid:
            raise ValueError('Original node replaced')
        return command([provider, 'exec', '-i', cid]+args, value, timeout)
    def read(path):
        run(['test', '-f', path])
        run(['test', '!', '-L', path])
        return run(['cat', path])
    lock = offline_runtime.load_lock()
    marker = json.loads(read(offline_runtime.PREFIX+'/ai-sdlc-owner.json'))
    if marker != {'clusterUID': cluster_uid, 'archiveSHA256': lock['sha256']}:
        raise ValueError('Pinned gVisor installation ownership differs')
    for name, entry in lock['files'].items():
        if run(['sha256sum', offline_runtime.PREFIX+'/'+name]).split()[0] != entry['sha256']:
            raise ValueError('Pinned gVisor binary changed')
    if read(offline_runtime.SHIM_CONFIG) != (offline_runtime.MANIFESTS/'runsc.toml').read_text():
        raise ValueError('Offline isolation configuration changed')
    before = read(offline_runtime.NODE_CONFIG)
    fragment = (MANIFESTS/'containerd.toml').read_text()
    desired = node_configuration(before, fragment)
    shim = (MANIFESTS/'runsc.toml').read_text()
    # Missing file is distinguishable from inability to inspect it.
    files = run(['find', '/etc/containerd', '-maxdepth', '1', '-name', 'ai-sdlc-browser.toml', '-print']).splitlines()
    if files and (files != [SHIM_CONFIG] or read(SHIM_CONFIG) != shim):
        raise ValueError('Conflicting browser shim configuration')
    existing = command(kubectl+['get', 'runtimeclass', HANDLER, '--ignore-not-found', '-o', 'json'])
    if existing:
        obj = json.loads(existing)
        if obj.get('handler') != HANDLER or obj.get('overhead') or obj.get('scheduling') or obj['metadata'].get('annotations', {}).get('ai-sdlc.io/cluster-uid') != cluster_uid:
            raise ValueError('Foreign browser RuntimeClass')
    if not files:
        run(['tee', SHIM_CONFIG], shim)
    if desired != before:
        stage = offline_runtime.NODE_CONFIG+'.browser-'+secrets.token_hex(8)
        try:
            run(['tee', stage], desired)
            run(['containerd', '--config', stage, 'config', 'dump'])
            if read(offline_runtime.NODE_CONFIG) != before:
                raise ValueError('Containerd configuration changed concurrently')
            run(['mv', '-T', stage, offline_runtime.NODE_CONFIG])
            try:
                run(['systemctl', 'restart', 'containerd'], timeout=60)
            except subprocess.SubprocessError:
                run(['tee', stage], before)
                run(['mv', '-T', stage, offline_runtime.NODE_CONFIG])
                run(['systemctl', 'restart', 'containerd'], timeout=60)
                raise ValueError('Browser runtime restart failed; original config restored')
        finally:
            run(['rm', '-f', '--', stage])
    command(kubectl+['wait', '--for=condition=Ready', 'node', node, '--timeout=60s'], timeout=70)
    if not existing:
        obj = json.loads((MANIFESTS/'runtimeclass.json').read_text())
        obj['metadata']['annotations'] = {'ai-sdlc.io/cluster-uid': cluster_uid}
        command(kubectl+['create', '-f', '-'], json.dumps(obj))
    final = json.loads(command(kubectl+['get', 'runtimeclass', HANDLER, '-o', 'json']))
    if (final.get('handler') != HANDLER or final.get('overhead') or final.get('scheduling')
            or final['metadata'].get('annotations', {}).get('ai-sdlc.io/cluster-uid') != cluster_uid
            or existing and final['metadata']['uid'] != json.loads(existing)['metadata']['uid']):
        raise ValueError('Browser RuntimeClass changed during verification')
    if read(offline_runtime.NODE_CONFIG) != desired or read(offline_runtime.SHIM_CONFIG) != (offline_runtime.MANIFESTS/'runsc.toml').read_text():
        raise ValueError('Runtime installation changed during verification')
    return {'runtimeClass': HANDLER, 'runtimeClassUID': final['metadata']['uid'], 'clusterUID': cluster_uid,
            'configurationSHA256': offline_runtime.sha256(shim.encode()), 'network': 'sandbox',
            'nodeConfigurationBeforeSHA256': offline_runtime.sha256(before.encode()),
            'nodeConfigurationAfterSHA256': offline_runtime.sha256(desired.encode()),
            'offlineRuntimeUnchanged': True, 'networkPolicyValidated': False, 'autonomousE2E': False}
