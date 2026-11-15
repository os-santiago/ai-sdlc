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
from types import SimpleNamespace
import urllib.request

import github_client
import network_probe
import install_config

ROOT = Path(__file__).resolve().parents[1]
LOCAL = ROOT / 'runtime/k8s/deploy/local'
CHART = ROOT / 'runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime'
COMPONENT_NAMES = {'intake': 'intake', 'admissionController': 'admission-controller',
                   'orchestrator': 'orchestrator', 'worker': 'worker',
                   'releaseManager': 'release-manager', 'relay': 'relay'}
COMPONENTS = tuple(COMPONENT_NAMES)
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
        raise ValueError('The image lock must contain exactly the declared runtime components')
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


def configure_identities(kubectl, provider, lock, cluster_uid, repository, environment, freeze=None, candidate=False):
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
    if freeze is not None:
        if type(freeze) is not bool or material['policy'].get('autonomy') not in ('shadow', 'auto-pr'):
            raise ValueError('Unsupported installation authority ceiling')
        material['policy']['freeze'] = freeze
        material['policy']['autonomy'] = 'auto-pr' if candidate else 'shadow'
        # Candidate mode grants only issue ingress and isolated execution.
        # Candidate authority is restricted to intake, planning and execution.
        if candidate:
            material['policy']['grants'] = [g for g in material['policy']['grants']
                if g['operation'] in ('admit-issue', 'plan-admission', 'execute-work')]
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


def configure_github_client(kubectl, cluster_uid, repository, refresh=False):
    secrets_api = SimpleNamespace(owned_secret=owned_secret, read_resource=read_resource,
                                  create_owned_secret=create_owned_secret, run=run)
    return github_client.configure(secrets_api, kubectl, cluster_uid, repository, refresh)


def shadow_intake_config(kubectl, cluster_uid, repository):
    current = owned_secret(kubectl, 'ai-sdlc-github-client', cluster_uid)
    if current is None:
        raise ValueError('Shadow intake requires an explicitly imported GitHub client; run credentials --github-auth gh-cli first')
    # Revalidate the existing credential without falling back to ambient gh.
    configure_github_client(kubectl, cluster_uid, repository)
    if owned_secret(kubectl, 'ai-sdlc-github-client', cluster_uid) != current:
        raise ValueError('GitHub client changed while deriving intake authorization; retry installation')
    credential = json.loads(current['credential.json'])
    policy = json.loads((LOCAL / 'intake-policy.json').read_text())
    if (set(policy) != {'version', 'baseBranch', 'triggerLabel', 'authorizedActorSource', 'pollSeconds', 'maxPages'}
            or policy.pop('authorizedActorSource') != 'github-client'):
        raise ValueError('Unsupported intake actor source')
    policy.update(repository=repository, authorizedActorIDs=[credential['actorID']])
    return {'configJSON': json.dumps(policy, sort_keys=True)}


def main(argv=None):
    selected = sys.argv[1:] if argv is None else argv
    if selected and selected[0] == 'supply-chain':
        import supply_chain_install
        return supply_chain_install.main(selected[1:])
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['plan', 'local', 'status', 'tools', 'host', 'credentials', 'sandbox', 'supply-chain'])
    parser.add_argument('--configure-host', action='store_true', help='Raise inotify limits to committed minima for this boot; requires root')
    parser.add_argument('--config', type=Path, help='Reviewed local installation JSON; credentials remain explicit CLI choices')
    parser.add_argument('--cluster')
    parser.add_argument('--provider', choices=['auto', 'docker', 'podman'])
    parser.add_argument('--images', type=Path)
    parser.add_argument('--timeout', default='10m')
    parser.add_argument('--repository')
    parser.add_argument('--environment')
    parser.add_argument('--profile', choices=['maintenance', 'shadow', 'candidate'])
    parser.add_argument('--github-auth', choices=['none', 'gh-cli'], default='none')
    parser.add_argument('--refresh-github-credential', action='store_true')
    parser.add_argument('--codex-auth-file', type=Path, help='Explicit local Codex authentication source for the candidate sidecar Secret')
    parser.add_argument('--refresh-model-credential', action='store_true')
    args = parser.parse_args(argv)
    config_digest = install_config.resolve(args)
    defaults = dict(cluster='ai-sdlc', provider='auto', images=LOCAL / 'images.lock.json',
                    repository='os-santiago/ai-sdlc', environment='ai-sdlc-production', profile='maintenance')
    for key, value in defaults.items():
        if getattr(args, key) is None:
            setattr(args, key, value)
    if args.profile in ('shadow', 'candidate') and args.command not in ('plan', 'local'):
        raise ValueError('--profile shadow/candidate is valid only with plan or local')
    if (args.codex_auth_file is not None or args.refresh_model_credential) and (args.command != 'local' or args.profile != 'candidate'):
        raise ValueError('Model credentials are only accepted by an explicit local candidate installation')
    if args.refresh_model_credential and args.codex_auth_file is None:
        raise ValueError('Model credential refresh requires an explicit authentication file')
    if args.github_auth != 'none' and args.command not in ('plan', 'local', 'credentials'):
        raise ValueError('--github-auth is valid only with plan, local or credentials')
    if args.command == 'credentials' and args.github_auth != 'gh-cli':
        raise ValueError('Select --github-auth gh-cli explicitly')
    if args.refresh_github_credential and (args.command not in ('local', 'credentials') or args.github_auth != 'gh-cli'):
        raise ValueError('Credential refresh requires local or credentials with --github-auth gh-cli')
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
                 '-f', LOCAL / 'values.yaml', '-f', LOCAL / 'profiles' / (args.profile + '.json'), '-f', args.images]
    execution_values = None
    if args.profile == 'candidate':
        import candidate_install
        execution_values = candidate_install.desired_values(ROOT, args.images, image_lock(args.images))
    with tempfile.NamedTemporaryFile(mode='w', suffix='.json') as preview:
        json.dump(execution_values or {}, preview); preview.flush()
        run(helm_args + ['-f', preview.name], stdout=subprocess.DEVNULL)
    if args.command == 'plan':
        print(json.dumps({'cluster': args.cluster, 'namespace': NAMESPACE,
                          'chart': str(CHART.relative_to(ROOT)), 'mode': args.profile,
                          'images': image_lock(args.images), 'deliveryEnabled': False,
                          'githubAuth': args.github_auth, 'repository': args.repository,
                          'environment': args.environment, 'configurationDigest': config_digest}, indent=2))
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
    if args.command in ('status', 'credentials', 'sandbox') and args.cluster not in clusters:
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
        config = read_resource(kubectl, 'configmap', 'ai-sdlc-config')
        mode = (config or {}).get('data', {}).get('AI_SDLC_RUNTIME_MODE', 'unknown')
        print('Installation mode: ' + mode + '. Autonomous issue-to-production delivery: unavailable.')
        return 0
    if args.command == 'credentials':
        policy = read_resource(kubectl, 'configmap', 'ai-sdlc-authority')
        if (policy is None or policy.get('metadata', {}).get('annotations', {}).get('ai-sdlc.io/cluster-uid') != cluster_uid
                or json.loads(policy.get('data', {}).get('policy.json', '{}')).get('repository') != args.repository):
            raise ValueError('Install matching repository authority before configuring credentials')
        print(json.dumps(configure_github_client(kubectl, cluster_uid, args.repository,
                                                args.refresh_github_credential), indent=2))
        return 0
    if args.command == 'sandbox':
        import offline_runtime
        policy = read_resource(kubectl, 'configmap', 'ai-sdlc-authority')
        scope = json.loads((policy or {}).get('data', {}).get('policy.json', '{}'))
        if (policy is None or policy.get('metadata', {}).get('annotations', {}).get('ai-sdlc.io/cluster-uid') != cluster_uid
                or scope.get('repository') != args.repository or scope.get('environment') != args.environment):
            raise ValueError('Install matching authority scope before configuring the offline runtime')
        lock = image_lock(args.images)
        result = offline_runtime.install(kubectl, provider, kind, args.cluster, cluster_uid,
                                         cache / 'offline-runtime',
                                         lock['image']['repository'] + '@' + lock['components']['worker']['digest'])
        (state / 'offline-runtime.json').write_text(json.dumps(result, indent=2) + '\n')
        print(json.dumps(result, indent=2))
        return 0
    # kind's node readiness alone does not establish service networking or
    # dynamic storage. Fail here before consuming the bounded migration window.
    run(kubectl + ['wait', '--for=condition=Ready', 'pods', '--all', '-n', 'kube-system',
                   '--timeout', args.timeout])
    run(kubectl + ['rollout', 'status', 'deployment/local-path-provisioner',
                   '-n', 'local-path-storage', '--timeout', args.timeout])
    run(kubectl + ['apply', '-f', LOCAL / 'namespace.yaml'])
    if args.github_auth == 'gh-cli':
        configure_github_client(kubectl, cluster_uid, args.repository, args.refresh_github_credential)
    intake_values = None
    if args.profile in ('shadow', 'candidate'):
        intake_values = shadow_intake_config(kubectl, cluster_uid, args.repository)
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
                                         cluster_uid, args.repository, args.environment,
                                         freeze=args.profile == 'maintenance', candidate=args.profile == 'candidate')
    if intake_values is not None:
        public_values['intake'] = intake_values
    if args.profile == 'candidate':
        import candidate_install
        import offline_runtime
        lock = image_lock(args.images)
        execution_values["candidate"]["credentialRevision"] = candidate_install.model_credentials(kubectl, cluster_uid, args.codex_auth_file, args.refresh_model_credential)
        offline_runtime.install(kubectl, provider, kind, args.cluster, cluster_uid, cache,
                                lock['image']['repository'] + '@' + lock['components']['worker']['digest'])
        execution_values['candidate']['apiServerCIDRs'] = candidate_install.api_cidrs(kubectl)
        public_values.update(execution_values)
    # This temporary Helm input contains public verification keys only. Helm
    # owns workloads and the policy ConfigMap; private data stays in Secrets.
    with tempfile.NamedTemporaryFile(mode='w', suffix='.json') as values:
        json.dump(public_values, values)
        values.flush()
        run([binaries['helm'], 'upgrade', '--install', 'ai-sdlc', CHART,
             '--kubeconfig', kubeconfig, '--kube-context', 'kind-' + args.cluster,
             '--namespace', NAMESPACE, '-f', LOCAL / 'values.yaml',
             '-f', LOCAL / 'profiles' / (args.profile + '.json'), '-f', args.images,
             # Poll readiness during recovery rather than failing immediately
             # on a Deployment's stale ProgressDeadlineExceeded condition.
             '-f', values.name, '--wait=legacy', '--wait-for-jobs', '--timeout', args.timeout])
    run(kubectl + ['rollout', 'status', 'statefulset/ai-sdlc-postgres', '-n', NAMESPACE,
                   '--timeout', args.timeout])
    for name in COMPONENT_NAMES.values():
        run(kubectl + ['rollout', 'status', 'deployment/ai-sdlc-' + name, '-n', NAMESPACE,
                       '--timeout', args.timeout])
    lock = image_lock(args.images)
    network = network_probe.verify(kubectl, lock['image']['repository'] + '@' + lock['components']['worker']['digest'])
    report = {'cluster': args.cluster, 'namespace': NAMESPACE, 'provider': provider,
              'repository': args.repository, 'environment': args.environment,
              'configurationDigest': config_digest,
              'mode': args.profile, 'installationReady': True, 'deliveryEnabled': False,
              'issueIntakeEnabled': args.profile in ('shadow', 'candidate'),
              'candidateExecutionEnabled': args.profile == 'candidate',
              'networkPolicyVerification': network,
              'images': image_lock(args.images)}
    (state / 'installation.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report, indent=2))
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        # Do not serialize process input (which can contain a Secret).
        print('Installation failed: ' + str(error), file=sys.stderr)
        sys.exit(1)
