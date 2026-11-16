"""Frozen runtime capability on an existing, installer-owned Konflux cluster."""
import fcntl
import json
import os
from pathlib import Path
import re
import secrets
import tempfile

import supply_chain_install

NAMESPACE = 'ai-sdlc-runtime'
CAPABILITY = 'frozen-runtime/v1'
LABELS = {'app.kubernetes.io/part-of': 'ai-sdlc',
          **{'pod-security.kubernetes.io/' + key: 'restricted'
             for key in ('enforce', 'audit', 'warn')}}


def validate_options(args):
    if args.platform != 'konflux':
        return
    if (args.command == 'status' and args.repository == 'os-santiago/saferus'
            and args.environment == 'ai-sdlc-production' and args.github_auth == 'none'
            and args.codex_auth_file is None and not args.refresh_github_credential and not args.refresh_model_credential):
        return
    if getattr(args, 'build_bundle', None) is not None:
        if (args.command not in ('plan', 'local') or args.profile != 'verification'
                or args.repository != 'os-santiago/saferus' or args.environment != 'ai-sdlc-production'
                or not args.merge or not args.publication or not args.independent_review):
            raise ValueError('Build capability requires reviewed Saferus test-production scope')
        return
    if (args.command not in ('plan', 'local', 'status') or args.profile != 'maintenance'
            or args.repository != 'os-santiago/saferus' or args.environment != 'ai-sdlc-production'
            or args.github_auth != 'none' or args.codex_auth_file is not None
            or args.refresh_github_credential or args.refresh_model_credential
            or args.merge or args.publication or args.independent_review):
        raise ValueError('Konflux runtime capability currently supports only frozen Saferus test-production plan/local/status without credentials')


def lock_state(state, stack):
    if state.is_symlink():
        raise ValueError('Capability state directory cannot use symlinks')
    path = state / 'runtime-install.lock'
    fd = os.open(path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    stack.callback(os.close, fd)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        raise ValueError('Another runtime installation holds this cluster lock') from None


def select_provider(requested, state, cluster):
    for name in ('konflux-ownership.json', 'kubeconfig', 'runtime-capability.json'):
        if (state / name).is_symlink():
            raise ValueError('Konflux capability state cannot use symlinks')
    marker = state / 'konflux-ownership.json'
    if not marker.is_file() or (state / 'ownership.json').exists():
        raise ValueError('An existing exclusively owned Konflux cluster is required; run install.sh supply-chain local first')
    return supply_chain_install.select_provider(requested, marker, cluster)


def verify_cluster(run, kubectl, state, cluster, provider, cluster_uid):
    expected = {'version': 1, 'cluster': cluster, 'provider': provider,
                'clusterUID': cluster_uid, 'scope': 'isolated-konflux'}
    if not cluster_uid or json.loads((state / 'konflux-ownership.json').read_text()) != expected:
        raise ValueError('Konflux cluster identity differs from its ownership record')
    lock, _ = supply_chain_install.lockfile()
    version = json.loads(run(kubectl + ['version', '-o', 'json'], capture_output=True).stdout)
    pinned = re.search(r':(v[0-9]+\.[0-9]+\.[0-9]+)@sha256:', lock['nodeImage'])
    if pinned is None or version.get('serverVersion', {}).get('gitVersion') != pinned[1]:
        raise ValueError('Konflux server version differs from the reviewed node image')
    api = json.loads(run(kubectl + ['get', '--raw', '/apis/admissionregistration.k8s.io/v1'], capture_output=True).stdout)
    if not {'mutatingadmissionpolicies', 'mutatingadmissionpolicybindings'} <= {r.get('name') for r in api.get('resources', [])}:
        raise ValueError('Native admission v1 capabilities are unavailable')
    return version['serverVersion']['gitVersion']


def write_record(path, value):
    if path.is_symlink():
        raise ValueError('Capability record cannot use symlinks')
    with tempfile.NamedTemporaryFile(mode='w', dir=path.parent, delete=False) as output:
        temporary = Path(output.name)
        try:
            json.dump(value, output, sort_keys=True)
            output.write('\n')
            output.flush()
            os.fsync(output.fileno())
            os.replace(temporary, path)
            fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
        finally:
            temporary.unlink(missing_ok=True)


def namespace(run, kubectl, state, cluster_uid, create):
    """Persist create intent first; recover only its nonce, never another namespace."""
    path = state / 'runtime-capability.json'
    record = json.loads(path.read_text()) if path.exists() else None
    new_intent = record is None
    expected = {'version': 1, 'capability': CAPABILITY, 'clusterUID': cluster_uid,
                'namespace': NAMESPACE}
    if record is not None and (not isinstance(record, dict)
            or set(record) not in (set(expected) | {'nonce'}, set(expected) | {'nonce', 'namespaceUID'})
            or any(record.get(k) != v for k, v in expected.items())
            or not re.fullmatch('[a-f0-9]{48}', str(record.get('nonce', '')))
            or ('namespaceUID' in record and not re.fullmatch('[a-f0-9-]{36}', str(record['namespaceUID'])))):
        raise ValueError('Invalid frozen runtime capability record')
    raw = run(kubectl + ['get', 'namespace', NAMESPACE, '--ignore-not-found', '-o', 'json'], capture_output=True).stdout
    current = json.loads(raw) if raw.strip() else None
    if record is None:
        if current is not None or not create:
            raise ValueError('Runtime namespace has no matching capability; refusing adoption')
        record = dict(expected, nonce=secrets.token_hex(24))
        write_record(path, record)
    annotations = {'ai-sdlc.io/cluster-uid': cluster_uid,
                   'ai-sdlc.io/runtime-capability': CAPABILITY,
                   'ai-sdlc.io/runtime-create-nonce': record['nonce']}
    if current is None:
        # A lost POST ACK is ambiguous. A subsequent invocation may bind only
        # an existing exact-nonce namespace. It never retries this intent.
        if not new_intent or not create:
            raise ValueError('Recorded runtime namespace is absent; refusing recreation')
        current = json.loads(run(kubectl + ['create', '-f', '-', '-o', 'json'],
            input=json.dumps({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {
                'name': NAMESPACE, 'labels': LABELS, 'annotations': annotations}}), capture_output=True).stdout)
    meta = current.get('metadata', {})
    if (current.get('apiVersion') != 'v1' or current.get('kind') != 'Namespace'
            or meta.get('name') != NAMESPACE or meta.get('deletionTimestamp')
            or not re.fullmatch('[a-f0-9-]{36}', str(meta.get('uid', '')))
            or any(meta.get('annotations', {}).get(k) != v for k, v in annotations.items())
            or any(meta.get('labels', {}).get(k) != v for k, v in LABELS.items())
            or ('namespaceUID' in record and record['namespaceUID'] != meta['uid'])):
        raise ValueError('Runtime namespace identity or security policy changed')
    if 'namespaceUID' not in record:
        if not create:
            raise ValueError('Runtime capability needs installer reconciliation before status')
        record['namespaceUID'] = meta['uid']
        write_record(path, record)
    return dict(record)


def require_frozen(run, kubectl):
    for name, field, required in (('ai-sdlc-config', 'AI_SDLC_RUNTIME_MODE', 'maintenance'),
                                  ('ai-sdlc-authority', 'policy.json', None)):
        raw = run(kubectl + ['get', 'configmap', name, '-n', NAMESPACE,
                            '--ignore-not-found', '-o', 'json'], capture_output=True).stdout
        if not raw.strip():
            continue
        data = json.loads(raw).get('data', {})
        if name == 'ai-sdlc-config' and data.get('AI_SDLC_BUILD', 'false') != 'false':
            raise ValueError('Installed build capability requires a containment-preserving upgrade or teardown; automatic reconciliation is not supported')
        if required is not None:
            valid = data.get(field) == required
        else:
            valid = json.loads(data.get(field, '{}')).get('freeze') is True
        if not valid:
            raise ValueError('Existing runtime must already be frozen before capability reconciliation')
