"""Compile and verify approved build installation inputs without advancing work."""
import json
from pathlib import Path
import re
import secrets
import subprocess
from install_config import _unique


def load_bundle(path, lock):
    path = Path(path)
    if path.is_symlink() or not path.is_file():
        raise ValueError('Build desired state must be a regular file')
    with path.open('rb') as stream:
        raw = stream.read(131073)
    if len(raw) > 131072:
        raise ValueError('Build desired state exceeds bound')
    value = json.loads(raw, object_pairs_hook=_unique)
    if (not isinstance(value, dict) or set(value) != {'version', 'revision', 'controllerDigest', 'profile', 'slots', 'issueMaxAgeSeconds'}
            or value['version'] != 'ai-sdlc.build-desired/v1' or value['revision'] != lock['image']['tag']
            or not re.fullmatch(r'sha256:[a-f0-9]{64}', str(value['controllerDigest']))
            or not isinstance(value['profile'], dict)
            or not re.fullmatch(r'quay\.io/os-santiago/ai-sdlc@sha256:[a-f0-9]{64}', str(value['profile'].get('publisherImage', '')))
            or not isinstance(value['slots'], list)
            or not 1 <= len(value['slots']) <= 8
            or type(value['issueMaxAgeSeconds']) is not int or not 30 <= value['issueMaxAgeSeconds'] <= 300):
        raise ValueError('Invalid reviewed build desired state')
    return value


def component_call(provider, image, operation, value, node_id=None):
    if provider not in ('podman', 'docker') or not re.fullmatch(r'quay\.io/os-santiago/ai-sdlc@sha256:[a-f0-9]{64}', image):
        raise ValueError('Pinned reviewed migrator image required')
    if node_id is not None and not re.fullmatch(r'[a-f0-9]{64}', node_id):
        raise ValueError('Original node container ID required')
    nonce = secrets.token_hex(24)
    name = 'ai-sdlc-build-install-' + nonce
    network = 'container:' + node_id if node_id else 'none'
    try:
        result = subprocess.run([provider, 'run', '--rm', '--name', name, '-i',
            '--label', 'ai-sdlc.build-install=' + nonce, '--network', network,
            '--read-only', '--cap-drop=ALL', '--security-opt=no-new-privileges',
            '--memory=256m', '--cpus=1', '--log-driver=none', image, operation],
            input=json.dumps(value), capture_output=True, text=True, timeout=150, check=False)
        if result.returncode or len(result.stdout) > 262144:
            raise ValueError('Build installation compiler or preflight rejected its input')
        return json.loads(result.stdout)
    finally:
        # Private credentials travel only through stdin. Cleanup may use this
        # nonce to recover the original administrative read-only container.
        found = subprocess.run([provider, 'inspect', name], capture_output=True, text=True, timeout=10, check=False)
        if found.returncode == 0:
            actual = json.loads(found.stdout)[0]
            cid = actual.get('Id', '')
            if (not re.fullmatch(r'[a-f0-9]{64}', cid)
                    or actual.get('Config', {}).get('Labels', {}).get('ai-sdlc.build-install') != nonce):
                raise ValueError('Refusing cleanup of another installation container')
            subprocess.run([provider, 'rm', '-f', '-v', cid], capture_output=True, check=True, timeout=20)
        else:
            names = subprocess.run([provider, 'ps', '-a', '--format', '{{.Names}}'], capture_output=True, text=True, timeout=10, check=True).stdout.splitlines()
            if name in names:
                raise ValueError('Installation container absence is unproven')


def preflight(run, kubectl, provider, cluster, lock, bundle):
    # Read only. The desired profile already commits to the approved trust
    # ConfigMap fingerprint; equality below prevents silently learning drift.
    node = json.loads(run([provider, 'inspect', cluster + '-control-plane'], capture_output=True, timeout=15).stdout)[0]
    if node.get('Config', {}).get('Labels', {}).get('io.x-k8s.kind.cluster') != cluster:
        raise ValueError('Build node does not belong to the selected installation')
    cfg = json.loads(run(kubectl + ['config', 'view', '--raw', '--minify', '-o', 'json'], capture_output=True, timeout=15).stdout)
    cluster_config, user = cfg['clusters'][0]['cluster'], cfg['users'][0]['user']
    client = {'APIServer': 'https://127.0.0.1:6443', 'CA': cluster_config['certificate-authority-data'],
              'ClientCertificate': user['client-certificate-data'], 'ClientKey': user['client-key-data']}
    trust = json.loads(run(kubectl + ['get', 'configmap', 'build-trust', '-n', 'ai-sdlc-builds', '-o', 'json'], capture_output=True, timeout=15).stdout)
    approved = json.loads(trust.get('data', {}).get('trust.json', '{}'))
    profile = bundle['profile']
    admission = {key: profile[key] for key in ('clusterUID', 'namespaceUID', 'publisherImage', 'deniedTargets')}
    admission['version'] = 'ai-sdlc.build-admission-input/v1'
    image = lock['image']['repository'] + '@' + lock['migrator']['digest']
    observed = component_call(provider, image, 'observe-build-profile',
        {'client': client, 'slots': bundle['slots'], 'admission': admission, 'approvedTrust': approved}, node['Id'])
    if observed != profile:
        raise ValueError('Actual build installation differs from approved desired profile')


def values(provider, lock, bundle, public_values, intake_values):
    inner = json.loads(public_values['authority']['policyJSON'])
    outer = json.loads(public_values['build']['outerPolicyJSON'])
    image = lock['image']['repository'] + '@' + lock['migrator']['digest']
    compiled = component_call(provider, image, 'compile-build-installation',
        {'inner': inner, 'outer': outer, 'profile': bundle['profile'], 'slots': bundle['slots'],
         'intake': json.loads(intake_values['configJSON']), 'issueMaxAgeSeconds': bundle['issueMaxAgeSeconds']})
    if compiled.get('inner') != inner or compiled.get('outer') != outer or compiled.get('profile') != bundle['profile']:
        raise ValueError('Build compiler changed approved input')
    return {'enabled': True, 'digest': bundle['controllerDigest'],
            'configJSON': json.dumps(compiled, sort_keys=True), 'outerPolicyJSON': public_values['build']['outerPolicyJSON']}
