#!/usr/bin/env python3
"""Install the committed Helm desired state; never advance delivery work."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import secrets
import shutil
import subprocess
import sys
import tarfile
import tempfile
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
LOCAL = ROOT / 'runtime/k8s/deploy/local'
CHART = ROOT / 'runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime'
COMPONENTS = ('admissionController', 'orchestrator', 'worker', 'releaseManager', 'relay')
NAMESPACE = 'ai-sdlc-runtime'
ROLES = ('intake', 'admission-controller', 'orchestrator', 'worker', 'verifier', 'release-manager', 'relay', 'deployer')


def run(args, **kwargs):
    return subprocess.run([str(a) for a in args], check=True, text=True, **kwargs)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def host_prerequisites(configure=False, proc_root=Path('/proc/sys/fs/inotify')):
    policy = json.loads((LOCAL / 'host-prerequisites.json').read_text())
    limits = policy.get('inotify', {})
    if policy.get('version') != 1 or set(limits) != {'max_user_instances', 'max_user_watches'}:
        raise ValueError('Unsupported host prerequisite policy')
    for name, minimum in limits.items():
        if type(minimum) is not int or not 1 <= minimum <= 2097152:
            raise ValueError('Invalid inotify prerequisite value')
    observed = {name: int((proc_root / name).read_text().strip()) for name in limits}
    for name, minimum in limits.items():
        current = observed[name]
        if current < minimum:
            if not configure:
                raise ValueError('Host inotify limits are below the committed minimum. Run ./install.sh host --configure-host as root, or apply deploy/local/host-prerequisites.json with your host configuration manager.')
            if os.geteuid() != 0:
                raise ValueError('Host prerequisite changes require root; the installer does not invoke sudo')
            # Monotonic, current-boot changes only. Never lower an existing limit
            # or rewrite a host-managed persistent sysctl configuration.
            run(['sysctl', '-w', 'fs.inotify.' + name + '=' + str(minimum)], stdout=subprocess.DEVNULL)
            observed[name] = int((proc_root / name).read_text().strip())
            if observed[name] < minimum:
                raise ValueError('Host prerequisite change was not applied')
    return observed


def tools(cache):
    if platform.system() != 'Linux' or platform.machine() not in ('x86_64', 'amd64'):
        raise ValueError('This release supports Linux amd64. Other platforms are not yet validated.')
    lock = json.loads((ROOT / 'tools/install-tools.lock.json').read_text())
    cache.mkdir(parents=True, exist_ok=True)
    binaries = {}
    for name, entry in lock.items():
        target = cache / (name + '-' + entry['version'])
        if not target.exists() or digest(target.read_bytes()) != entry['binarySha256']:
            print('Downloading verified ' + name + ' ' + entry['version'], file=sys.stderr, flush=True)
            with urllib.request.urlopen(entry['url'], timeout=120) as response:
                data = response.read()
            if digest(data) != entry['sha256']:
                raise ValueError('Download integrity check failed: ' + name)
            if entry.get('member'):
                with tempfile.TemporaryFile() as archive:
                    archive.write(data)
                    archive.seek(0)
                    with tarfile.open(fileobj=archive, mode='r:gz') as tar:
                        data = tar.extractfile(entry['member']).read()
            if digest(data) != entry['binarySha256']:
                raise ValueError('Binary integrity check failed: ' + name)
            with tempfile.NamedTemporaryFile(dir=cache, delete=False) as tmp:
                tmp.write(data)
                temporary = Path(tmp.name)
            temporary.chmod(0o755)
            temporary.replace(target)
        binaries[name] = str(target)
    return binaries


def image_lock(path):
    lock = json.loads(path.read_text())
    if not isinstance(lock, dict) or set(lock) != {'image', 'components', 'migrator'}:
        raise ValueError('Image lock requires image, components and migrator')
    if not isinstance(lock['image'], dict) or set(lock['image']) - {'repository', 'tag'}:
        raise ValueError('Image lock cannot override installation configuration')
    if 'tag' in lock['image'] and not re.fullmatch(r'[a-f0-9]{40}', lock['image']['tag']):
        raise ValueError('Image lock tag must record a full source commit SHA')
    if lock.get('image', {}).get('repository') != 'quay.io/os-santiago/ai-sdlc':
        raise ValueError('Unexpected image repository in lock')
    if set(lock.get('components', {})) != set(COMPONENTS):
        raise ValueError('The image lock must contain exactly five runtime components')
    for name in COMPONENTS:
        if not isinstance(lock['components'][name], dict) or set(lock['components'][name]) != {'digest'}:
            raise ValueError('Component lock accepts only a digest: ' + name)
        if not re.fullmatch(r'sha256:[a-f0-9]{64}', lock['components'][name].get('digest', '')):
            raise ValueError('Missing published image digest: ' + name)
    if not isinstance(lock['migrator'], dict) or set(lock['migrator']) != {'digest'} or not re.fullmatch(r'sha256:[a-f0-9]{64}', lock['migrator']['digest']):
        raise ValueError('Missing immutable migrator image digest')
    return lock


def check_ownership(marker, cluster, provider, cluster_uid):
    expected = {'cluster': cluster, 'provider': provider, 'clusterUID': cluster_uid}
    if not cluster_uid or not marker.exists() or json.loads(marker.read_text()) != expected:
        raise ValueError('Cluster identity differs from its ownership record; refusing to adopt or modify it.')


def read_resource(kubectl, kind, name):
    result = run(kubectl + ['get', kind, name, '-n', NAMESPACE,
                            '--ignore-not-found', '-o', 'json'], capture_output=True)
    return json.loads(result.stdout) if result.stdout.strip() else None


def owned_secret(kubectl, name, cluster_uid):
    resource = read_resource(kubectl, 'secret', name)
    if resource is None:
        return None
    if (resource.get('type') != 'Opaque'
            or resource.get('metadata', {}).get('annotations', {}).get('ai-sdlc.io/cluster-uid') != cluster_uid):
        raise ValueError('Refusing to adopt a Secret without matching installation ownership: ' + name)
    return {key: base64.b64decode(value, validate=True).decode('utf-8')
            for key, value in resource.get('data', {}).items()}


def create_owned_secret(kubectl, name, values, cluster_uid):
    resource = {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {
        'name': name, 'namespace': NAMESPACE,
        'labels': {'app.kubernetes.io/part-of': 'ai-sdlc'},
        'annotations': {'ai-sdlc.io/cluster-uid': cluster_uid}},
        'type': 'Opaque', 'stringData': values}
    run(kubectl + ['create', '-f', '-'], input=json.dumps(resource), stdout=subprocess.DEVNULL)


def component_uri(role, password):
    if role not in ROLES or not re.fullmatch(r'[A-Za-z0-9_-]{32,128}', password):
        raise ValueError('Invalid component database credential')
    return ('postgresql://aisdlc_' + role.replace('-', '_') + ':' + password
            + '@ai-sdlc-postgres:5432/ai_sdlc?sslmode=disable')


def identity_material(provider, image, repository, environment):
    # Disable the container log driver: private bootstrap stdout must exist only
    # in this captured pipe, never the host journal or container log files.
    result = run([provider, 'run', '--rm', '--network=none', '--log-driver=none',
                  image, 'bootstrap-identities', '--repository', repository,
                  '--environment', environment], capture_output=True)
    if len(result.stdout) > 128 * 1024:
        raise ValueError('Identity bootstrap exceeded its output bound')
    return json.loads(result.stdout)


def configure_identities(kubectl, provider, lock, cluster_uid, repository, environment):
    image = lock['image']['repository'] + '@' + lock['migrator']['digest']
    material = identity_material(provider, image, repository, environment)
    pending, passwords = [], {}
    for role in ROLES:
        name = 'ai-sdlc-identity-' + role
        current = owned_secret(kubectl, name, cluster_uid)
        if current is not None:
            if set(current) != {'identity.json', 'public-key.json'}:
                raise ValueError('Unexpected identity Secret shape: ' + name)
            identity = json.loads(current['identity.json'])
            public_key = json.loads(current['public-key.json'])
            if (not isinstance(identity, dict) or set(identity) != {'keyId', 'role', 'privateKey'}
                    or not all(isinstance(value, str) and value for value in identity.values())
                    or not isinstance(public_key, dict) or set(public_key) != {'role', 'value'}
                    or identity.get('role') != role or public_key.get('role') != role):
                raise ValueError('Identity Secret belongs to a different component: ' + name)
            material['identities'][role] = identity
            # The generated temporary key is never persisted when a key exists.
            material['policy']['keys'].pop(role + '-1', None)
            material['policy']['keys'][identity['keyId']] = public_key
        else:
            identity = material['identities'][role]
            pending.append((name, {'identity.json': json.dumps(identity, sort_keys=True),
                                   'public-key.json': json.dumps(material['policy']['keys'][identity['keyId']], sort_keys=True)}))
        name = 'ai-sdlc-database-' + role
        current = owned_secret(kubectl, name, cluster_uid)
        if current is not None:
            if set(current) != {'uri', 'password'} or current['uri'] != component_uri(role, current['password']):
                raise ValueError('Inconsistent component database Secret: ' + name)
            password = current['password']
        else:
            password = secrets.token_urlsafe(48)
            pending.append((name, {'password': password, 'uri': component_uri(role, password)}))
        passwords[role] = password
    if len(set(passwords.values())) != len(ROLES):
        raise ValueError('Component database credentials must be distinct')
    current_policy = read_resource(kubectl, 'configmap', 'ai-sdlc-authority')
    if current_policy is not None:
        if current_policy.get('metadata', {}).get('annotations', {}).get('ai-sdlc.io/cluster-uid') != cluster_uid:
            raise ValueError('Authority policy ownership does not match this installation')
        policy = json.loads(current_policy.get('data', {}).get('policy.json', '{}'))
        if policy.get('repository') != repository or policy.get('environment') != environment:
            raise ValueError('Existing authority scope differs; use its reviewed repository and environment')
        for identity in material['identities'].values():
            key_id = identity['keyId']
            if policy.get('keys', {}).get(key_id) != material['policy']['keys'][key_id]:
                raise ValueError('Existing authority policy and component identity differ; restore or rotate through reviewed configuration')
        material['policy'] = policy
    current_passwords = owned_secret(kubectl, 'ai-sdlc-database-roles', cluster_uid)
    if current_passwords is not None:
        if set(current_passwords) != {'passwords.json'} or json.loads(current_passwords['passwords.json']) != passwords:
            raise ValueError('Migrator credentials differ from the component database Secrets')
    else:
        pending.append(('ai-sdlc-database-roles', {'passwords.json': json.dumps(passwords, sort_keys=True)}))
    # Validate all key pairs and policy before creating any of this bootstrap's
    # resources. Neither subprocess output nor private input is printed.
    run([provider, 'run', '--rm', '-i', '--network=none', '--log-driver=none', image,
         'validate-bootstrap'], input=json.dumps(material), capture_output=True)
    for name, values in pending:
        create_owned_secret(kubectl, name, values, cluster_uid)
    return {'authority': {'policyJSON': json.dumps(material['policy'], sort_keys=True),
                          'installationUID': cluster_uid}}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['plan', 'local', 'status', 'tools', 'host'])
    parser.add_argument('--configure-host', action='store_true', help='Raise inotify limits to committed minima for this boot; requires root')
    parser.add_argument('--cluster', default='ai-sdlc')
    parser.add_argument('--provider', choices=['auto', 'docker', 'podman'], default='auto')
    parser.add_argument('--images', type=Path, default=LOCAL / 'images.lock.json')
    parser.add_argument('--timeout', default='10m')
    parser.add_argument('--repository', default='os-santiago/ai-sdlc')
    parser.add_argument('--environment', default='ai-sdlc-production')
    args = parser.parse_args(argv)
    if args.configure_host and args.command not in ('local', 'host'):
        raise ValueError('--configure-host is valid only with local or host')
    if args.command == 'host':
        print(json.dumps({'inotify': host_prerequisites(args.configure_host)}, indent=2))
        return 0
    if not re.fullmatch(r'[a-z][a-z0-9-]{0,35}', args.cluster):
        raise ValueError('Invalid cluster name')
    if not re.fullmatch(r'[1-9][0-9]*(s|m)', args.timeout):
        raise ValueError('Timeout must be positive seconds or minutes')
    if (not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', args.repository)
            or any(part in ('.', '..') for part in args.repository.split('/'))
            or not re.fullmatch(r'[a-z0-9][a-z0-9-]{0,62}', args.environment)):
        raise ValueError('Invalid authority repository or environment scope')
    cache = Path(os.environ.get('XDG_CACHE_HOME', Path.home() / '.cache')) / 'ai-sdlc/tools'
    if args.command != 'tools':
        image_lock(args.images)
    binaries = tools(cache)
    if args.command == 'tools':
        print(json.dumps(binaries, indent=2))
        return 0
    helm_args = [binaries['helm'], 'template', 'ai-sdlc', CHART, '--namespace', NAMESPACE,
                 '-f', LOCAL / 'values.yaml', '-f', args.images]
    run(helm_args, stdout=subprocess.DEVNULL)
    if args.command == 'plan':
        print(json.dumps({'cluster': args.cluster, 'namespace': NAMESPACE,
                          'chart': str(CHART.relative_to(ROOT)), 'mode': 'maintenance',
                          'images': image_lock(args.images), 'deliveryEnabled': False}, indent=2))
        return 0
    provider = args.provider
    if provider == 'auto':
        provider = next((p for p in ('docker', 'podman') if shutil.which(p)), None)
    if not provider or not shutil.which(provider):
        raise ValueError('Install Docker or Podman, then rerun; the installer does not change host privileges.')
    run([provider, 'info'], stdout=subprocess.DEVNULL)
    if args.command == 'local':
        host_prerequisites(args.configure_host)
    env = dict(os.environ, KIND_EXPERIMENTAL_PROVIDER=provider)
    state = Path(os.environ.get('XDG_STATE_HOME', Path.home() / '.local/state')) / 'ai-sdlc' / args.cluster
    state.mkdir(parents=True, exist_ok=True, mode=0o700)
    kubeconfig = state / 'kubeconfig'
    kind = binaries['kind']
    clusters = run([kind, 'get', 'clusters'], env=env, capture_output=True).stdout.splitlines()
    marker = state / 'ownership.json'
    if args.cluster in clusters and not marker.exists():
        raise ValueError('Cluster already exists without this installer ownership record; choose a different --cluster.')
    if args.command == 'status' and args.cluster not in clusters:
        raise ValueError('The requested local cluster does not exist')
    created = args.cluster not in clusters
    if created:
        run([kind, 'create', 'cluster', '--name', args.cluster, '--config', LOCAL / 'kind.yaml',
             '--kubeconfig', kubeconfig, '--wait', args.timeout], env=env)
    run([kind, 'export', 'kubeconfig', '--name', args.cluster, '--kubeconfig', kubeconfig], env=env)
    kubeconfig.chmod(0o600)
    kubectl = [binaries['kubectl'], '--kubeconfig', kubeconfig, '--context', 'kind-' + args.cluster]
    cluster_uid = run(kubectl + ['get', 'namespace', 'kube-system', '-o', 'jsonpath={.metadata.uid}'], capture_output=True).stdout.strip()
    if created:
        if not cluster_uid:
            raise ValueError('Cannot establish the created cluster identity')
        marker.write_text(json.dumps({'cluster': args.cluster, 'provider': provider, 'clusterUID': cluster_uid}) + '\n')
    else:
        check_ownership(marker, args.cluster, provider, cluster_uid)
    if args.command == 'status':
        run(kubectl + ['get', 'deployments,statefulsets,pods,pvc', '-n', NAMESPACE])
        print('Installation mode: maintenance. Autonomous issue delivery: unavailable.')
        return 0
    # kind's node readiness alone does not establish service networking or
    # dynamic storage. Fail here before consuming the bounded migration window.
    run(kubectl + ['wait', '--for=condition=Ready', 'pods', '--all', '-n', 'kube-system',
                   '--timeout', args.timeout])
    run(kubectl + ['rollout', 'status', 'deployment/local-path-provisioner',
                   '-n', 'local-path-storage', '--timeout', args.timeout])
    run(kubectl + ['apply', '-f', LOCAL / 'namespace.yaml'])
    # Bootstrap secret boundary: values refer to a Secret, never its contents.
    # An existing Secret is reused unchanged, including on repeat installs.
    result = run(kubectl + ['get', 'secret', 'ai-sdlc-postgres-app', '-n', NAMESPACE,
                            '--ignore-not-found', '-o', 'name'], capture_output=True)
    if not result.stdout.strip():
        password = secrets.token_urlsafe(48)
        secret = {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {
            'name': 'ai-sdlc-postgres-app', 'namespace': NAMESPACE,
            'labels': {'app.kubernetes.io/part-of': 'ai-sdlc'}}, 'type': 'Opaque',
            'stringData': {'password': password,
                           'uri': 'postgresql://ai_sdlc:' + password + '@ai-sdlc-postgres:5432/ai_sdlc?sslmode=disable'}}
        run(kubectl + ['create', '-f', '-'], input=json.dumps(secret), stdout=subprocess.DEVNULL)
    public_values = configure_identities(kubectl, provider, image_lock(args.images),
                                         cluster_uid, args.repository, args.environment)
    # This temporary Helm input contains public verification keys only. Helm
    # owns workloads and the policy ConfigMap; private data stays in Secrets.
    with tempfile.NamedTemporaryFile(mode='w', suffix='.json') as values:
        json.dump(public_values, values)
        values.flush()
        run([binaries['helm'], 'upgrade', '--install', 'ai-sdlc', CHART,
             '--kubeconfig', kubeconfig, '--kube-context', 'kind-' + args.cluster,
             '--namespace', NAMESPACE, '-f', LOCAL / 'values.yaml', '-f', args.images,
             '-f', values.name, '--wait', '--wait-for-jobs', '--timeout', args.timeout])
    run(kubectl + ['rollout', 'status', 'statefulset/ai-sdlc-postgres', '-n', NAMESPACE,
                   '--timeout', args.timeout])
    for name in ('admission-controller', 'orchestrator', 'worker', 'release-manager', 'relay'):
        run(kubectl + ['rollout', 'status', 'deployment/ai-sdlc-' + name, '-n', NAMESPACE,
                       '--timeout', args.timeout])
    report = {'cluster': args.cluster, 'namespace': NAMESPACE, 'provider': provider,
              'mode': 'maintenance', 'installationReady': True, 'deliveryEnabled': False,
              'images': image_lock(args.images)}
    (state / 'installation.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report, indent=2))
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        # Do not serialize process input (which can contain a Secret).
        print('Installation failed: ' + str(error), file=sys.stderr)
        sys.exit(1)
