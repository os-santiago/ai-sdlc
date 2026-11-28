#!/usr/bin/env python3
"""Install the committed Helm desired state; never advance delivery work."""
import argparse
from contextlib import ExitStack
import base64
import copy
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
import project_install
import github_merge_policy
import konflux_runtime_install
import openshift_install
import build_runtime_install
import kind_nodes
import tekton_ci_install

ROOT = Path(__file__).resolve().parents[1]
LOCAL = ROOT / 'runtime/k8s/deploy/local'
CHART = ROOT / 'runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime'
COMPONENT_NAMES = {'intake': 'intake', 'admissionController': 'admission-controller',
                   'orchestrator': 'orchestrator', 'worker': 'worker',
                   'releaseManager': 'release-manager', 'relay': 'relay'}
COMPONENTS = tuple(COMPONENT_NAMES)
NAMESPACE = 'ai-sdlc-runtime'
ROLES = ('intake', 'admission-controller', 'orchestrator', 'worker', 'verifier', 'release-manager', 'relay', 'deployer')
ALL_ROLES = ROLES + ('build-controller',)
# The optional read-only flow console observer (docs/operations/flow-console.md).
# It is not a workflow component: no identity, only a database login that the
# migrator provisions while passwords.json names it.
CONSOLE_ROLE = 'flow-console'
CONSOLE_SECRET = 'ai-sdlc-flow-console-database'
CONSOLE_SERVICE = 'ai-sdlc-flow-console'


def run(args, **kwargs):
    return subprocess.run([str(a) for a in args], check=kwargs.pop('check', True), text=True, **kwargs)


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


# Cluster tools every installation needs. cosign is pinned in the same lock but
# fetched only by `./install.sh tools` and the image-lock resolver.
CLUSTER_TOOLS = ('kind', 'helm', 'kubectl')


def tool_cache():
    return Path(os.environ.get('XDG_CACHE_HOME', Path.home() / '.cache')) / 'ai-sdlc/tools'


def tools(cache, names=None):
    if platform.system() != 'Linux' or platform.machine() not in ('x86_64', 'amd64'):
        raise ValueError('This release supports Linux amd64. Other platforms are not yet validated.')
    lock = json.loads((ROOT / 'tools/install-tools.lock.json').read_text())
    if names is not None:
        if not set(names) <= set(lock):
            raise ValueError('Requested tool is not pinned in install-tools.lock.json')
        lock = {name: lock[name] for name in names}
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



def extend_owned_role_secret(kubectl, cluster_uid, previous, values):
    """Append the reviewed role using Kubernetes UID/resourceVersion CAS."""
    resource = read_resource(kubectl, 'secret', 'ai-sdlc-database-roles')
    meta = (resource or {}).get('metadata', {})
    if (resource is None or resource.get('type') != 'Opaque'
            or meta.get('annotations', {}).get('ai-sdlc.io/cluster-uid') != cluster_uid
            or not meta.get('uid') or not meta.get('resourceVersion')):
        raise ValueError('Migrator credential ownership or version unavailable')
    data = {key: base64.b64decode(value, validate=True).decode('utf-8')
            for key, value in resource.get('data', {}).items()}
    if data != previous:
        raise ValueError('Migrator credentials changed during additive upgrade')
    resource['data'] = {key: base64.b64encode(value.encode()).decode() for key, value in values.items()}
    # Replace, never apply: stale versions fail without overwriting a new owner.
    run(kubectl + ['replace', '-f', '-'], input=json.dumps(resource),
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def component_uri(role, password):
    if role not in ALL_ROLES or not re.fullmatch(r'[A-Za-z0-9_-]{32,128}', password):
        raise ValueError('Invalid component database credential')
    return ('postgresql://aisdlc_' + role.replace('-', '_') + ':' + password
            + '@ai-sdlc-postgres:5432/ai_sdlc?sslmode=disable')


def console_uri(password):
    if not re.fullmatch(r'[A-Za-z0-9_-]{32,128}', password):
        raise ValueError('Invalid flow console database credential')
    return 'postgresql://aisdlc_flow_console:' + password + '@ai-sdlc-postgres:5432/ai_sdlc?sslmode=disable'


def console_images(path, lock):
    """Read the reviewed console image pin written by install_images.py
    --include-console. It must come from the same signed main run (revision)
    as the installation's image lock."""
    try:
        document = json.loads(Path(path).read_text())
    except (OSError, ValueError):
        raise ValueError('Flow console needs console-images.json beside the image lock (tools/install_images.py --include-console) or --console-images') from None
    if (not isinstance(document, dict) or set(document) != {'version', 'revision', 'digest'}
            or document['version'] != 'ai-sdlc.console-images/v1'
            or not isinstance(document['digest'], str) or not re.fullmatch(r'sha256:[0-9a-f]{64}', document['digest'])):
        raise ValueError('Invalid flow console image pin')
    if document['revision'] != lock.get('image', {}).get('tag'):
        raise ValueError('Flow console image pin is from a different publication than the image lock')
    return document['digest']


def remove_console_credential(kubectl, cluster_uid, installer=None):
    """After the migrator ran without the flow-console entry (role set to
    NOLOGIN, privileges revoked), delete the console's URL Secret."""
    api = installer or SimpleNamespace(read_resource=read_resource, run=run)
    resource = api.read_resource(kubectl, 'secret', CONSOLE_SECRET)
    # Delete only this installation's own Secret; leave any other untouched.
    if resource is None or resource.get('metadata', {}).get('annotations', {}).get('ai-sdlc.io/cluster-uid') != cluster_uid:
        return False
    api.run(kubectl + ['delete', 'secret', CONSOLE_SECRET, '-n', NAMESPACE, '--wait=true'], stdout=subprocess.DEVNULL)
    return True


def identity_material(provider, image, repository, environment):
    # Disable the container log driver: private bootstrap stdout must exist only
    # in this captured pipe, never the host journal or container log files.
    result = run([provider, 'run', '--rm', '--network=none', '--log-driver=none',
                  image, 'bootstrap-identities', '--repository', repository,
                  '--environment', environment], capture_output=True)
    if len(result.stdout) > 128 * 1024:
        raise ValueError('Identity bootstrap exceeded its output bound')
    return json.loads(result.stdout)


def configure_identities(kubectl, provider, lock, cluster_uid, repository, environment, freeze=None, candidate=False, verification=False, publication=False, merge=False, require_build=False, build=False, installer=None, review=False, project_manifest=None, flow_console=False):
    api = installer or SimpleNamespace(owned_secret=owned_secret, read_resource=read_resource,
        create_owned_secret=create_owned_secret, run=run)
    if build and (not merge or repository != 'os-santiago/saferus' or environment != 'ai-sdlc-production'):
        raise ValueError('Build requires reviewed Saferus merge and test production')
    review = review or publication
    if review and not verification:
        raise ValueError("Independent review requires verification")
    require_build = require_build or build
    if merge and not publication:
        raise ValueError('Merge requires verified publication')
    if publication and (not candidate or not verification or not project_install.reviewed(repository, project_manifest)):
        raise ValueError('Invalid publication authority scope')
    image = lock['image']['repository'] + '@' + lock['migrator']['digest']
    material = identity_material(provider, image, repository, environment)
    supplied_roles = set(material.get('identities', {}))
    if supplied_roles not in (set(ROLES), set(ALL_ROLES)) or (require_build and supplied_roles != set(ALL_ROLES)):
        raise ValueError('Pinned migrator image does not support the required component identity set')
    roles = ALL_ROLES if supplied_roles == set(ALL_ROLES) else ROLES
    reviewed_grants = material['policy']['grants']
    if merge and not any(g.get('operation') == 'merge-publication' for g in reviewed_grants):
        raise ValueError('Pinned migrator image does not support the merge capability')
    pending, passwords = [], {}
    password_upgrade = None
    preserved_inner_policy = None
    for role in roles:
        name = 'ai-sdlc-identity-' + role
        current = api.owned_secret(kubectl, name, cluster_uid)
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
        current = api.owned_secret(kubectl, name, cluster_uid)
        if current is not None:
            if set(current) != {'uri', 'password'} or current['uri'] != component_uri(role, current['password']):
                raise ValueError('Inconsistent component database Secret: ' + name)
            password = current['password']
        else:
            password = secrets.token_urlsafe(48)
            pending.append((name, {'password': password, 'uri': component_uri(role, password)}))
        passwords[role] = password
    # The flow console's password: same generator, same custody (its own
    # URL Secret plus the same passwords.json entry). Without the option the
    # entry is dropped, and the migrator disables the observer role.
    if flow_console:
        # Only consulted with the option: a foreign Secret of that name never
        # blocks an installation without the console.
        current = api.owned_secret(kubectl, CONSOLE_SECRET, cluster_uid)
        if current is not None:
            if set(current) != {'uri', 'password'} or current['uri'] != console_uri(current['password']):
                raise ValueError('Inconsistent flow console database Secret')
            password = current['password']
        else:
            password = secrets.token_urlsafe(48)
            pending.append((CONSOLE_SECRET, {'password': password, 'uri': console_uri(password)}))
        passwords[CONSOLE_ROLE] = password
    if len(set(passwords.values())) != len(passwords):
        raise ValueError('Component database credentials must be distinct')
    current_policy = api.read_resource(kubectl, 'configmap', 'ai-sdlc-authority')
    if current_policy is not None:
        if current_policy.get('metadata', {}).get('annotations', {}).get('ai-sdlc.io/cluster-uid') != cluster_uid:
            raise ValueError('Authority policy ownership does not match this installation')
        policy = json.loads(current_policy.get('data', {}).get('policy.json', '{}'))
        if policy.get('repository') != repository or policy.get('environment') != environment:
            raise ValueError('Existing authority scope differs; use its reviewed repository and environment')
        preserved_inner_policy = json.loads(json.dumps(policy))
        for identity in material['identities'].values():
            key_id = identity['keyId']
            # The new role is additive. Existing identities are never rotated.
            if identity['role'] == 'build-controller' and key_id not in policy.get('keys', {}):
                if any(key.get('role') == 'build-controller' for key in policy.get('keys', {}).values()):
                    raise ValueError('Conflicting build-controller authority identity')
                policy['keys'][key_id] = material['policy']['keys'][key_id]
            if policy.get('keys', {}).get(key_id) != material['policy']['keys'][key_id]:
                raise ValueError('Existing authority policy and component identity differ; restore or rotate through reviewed configuration')
        material['policy'] = policy
    if freeze is not None:
        if type(freeze) is not bool or material['policy'].get('autonomy') not in ('shadow', 'auto-pr', 'auto-merge-low'):
            raise ValueError('Unsupported installation authority ceiling')
        material['policy']['freeze'] = freeze
        material['policy']['autonomy'] = 'auto-merge-low' if merge else ('auto-pr' if candidate else 'shadow')
        # Candidate mode grants only issue ingress and isolated execution.
        # Candidate authority is restricted to intake, planning and execution.
        if candidate:
            material['policy']['grants'] = [g for g in reviewed_grants
                if g['operation'] in ('admit-issue', 'plan-admission', 'execute-work') or (verification and g['operation'] == 'verify-artifact') or (review and g['operation'] in ('repair-candidate', 'stop-rejected-work')) or (publication and g['operation'] == 'publish-candidate') or (merge and g['operation'] == 'merge-publication')]
    current_passwords = api.owned_secret(kubectl, 'ai-sdlc-database-roles', cluster_uid)
    if current_passwords is not None:
        if set(current_passwords) != {'passwords.json'}:
            raise ValueError('Migrator credentials differ from the component database Secrets')
        previous = json.loads(current_passwords['passwords.json'])
        if previous != passwords:
            # The console entry may be added or dropped; when kept, it must
            # still match the console's own Secret.
            console_before = previous.get(CONSOLE_ROLE)
            if console_before is not None and CONSOLE_ROLE in passwords and console_before != passwords[CONSOLE_ROLE]:
                raise ValueError('Migrator credentials differ from the flow console database Secret')
            before = {role: value for role, value in previous.items() if role != CONSOLE_ROLE}
            after = {role: value for role, value in passwords.items() if role != CONSOLE_ROLE}
            if before != after and (set(before) != set(roles) - {'build-controller'}
                    or any(before[role] != after[role] for role in before)):
                raise ValueError('Migrator credentials differ from the component database Secrets')
            password_upgrade = (current_passwords, {'passwords.json': json.dumps(passwords, sort_keys=True)})
    else:
        pending.append(('ai-sdlc-database-roles', {'passwords.json': json.dumps(passwords, sort_keys=True)}))
    project_values = None
    if project_manifest is not None:
        # The pinned migrator compiles the manifest; the policy binds its digest.
        compiled = project_install.compile_installation(api.run, provider, image, material['policy'], project_manifest)
        project_values = project_install.bind(material['policy'], compiled)
    elif material['policy'].get('projectSHA256'):
        raise ValueError('Installed authority binds a project manifest; supply it with --project-manifest')
    # Validate all key pairs and policy before creating any of this bootstrap's
    # resources. Neither subprocess output nor private input is printed.
    api.run([provider, 'run', '--rm', '-i', '--network=none', '--log-driver=none', image,
         'validate-bootstrap'], input=json.dumps(material), capture_output=True)
    outer_policy = None
    if build:
        outer_policy = dict(material['policy'], autonomy='auto-deploy',
                            keys={key_id: key for key_id, key in material['policy']['keys'].items()
                                  if key['role'] in ('release-manager', 'build-controller', 'verifier', 'deployer', 'relay', 'orchestrator')},
                            grants=[grant for grant in reviewed_grants if grant['operation'] in
                                    ('build-merged', 'verify-built', 'promote-artifact', 'deploy-release', 'record-deployment', 'record-production')])
    # Bootstrap validation uses all public keys only in this private input.
    # The build identity is not added to the inner policy or its revision.
    for key_id, key in list(material['policy']['keys'].items()):
        if key.get('role') == 'build-controller':
            if preserved_inner_policy is not None and key_id in preserved_inner_policy.get('keys', {}):
                raise ValueError('Build identity must not belong to the inner policy')
            del material['policy']['keys'][key_id]
    if installer is not None and (pending or password_upgrade is not None):
        raise ValueError('Prepared configuration requires its original complete identities and database credentials')
    for name, values in pending:
        api.create_owned_secret(kubectl, name, values, cluster_uid)
    if password_upgrade is not None:
        extend_owned_role_secret(kubectl, cluster_uid, *password_upgrade)
    result = {'authority': {'policyJSON': json.dumps(material['policy'], sort_keys=True),
                            'installationUID': cluster_uid}}
    if outer_policy is not None:
        result['build'] = {'outerPolicyJSON': json.dumps(outer_policy, sort_keys=True)}
    if project_values is not None:
        result['project'] = project_values
    return result


def configure_github_client(kubectl, cluster_uid, repository, refresh=False, installer=None):
    secrets_api = installer or SimpleNamespace(owned_secret=owned_secret, read_resource=read_resource,
                                  create_owned_secret=create_owned_secret, run=run)
    return github_client.configure(secrets_api, kubectl, cluster_uid, repository, refresh)


def configure_publication_client(kubectl, cluster_uid, repository, auth, refresh=False, installer=None, project_manifest=None):
    api = installer or SimpleNamespace(owned_secret=owned_secret, read_resource=read_resource,
        create_owned_secret=create_owned_secret, run=run)
    name = 'ai-sdlc-publication-client'
    if not project_install.reviewed(repository, project_manifest):
        raise ValueError('Publication supports only reviewed Homedir, Saferus or an installed project manifest')
    if api.owned_secret(kubectl, name, cluster_uid) is None and auth != 'gh-cli':
        raise ValueError('Publication needs an explicitly imported client via --github-auth gh-cli')
    github_client.configure(api, kubectl, cluster_uid, repository, refresh, name=name)
    resource = api.read_resource(kubectl, 'secret', name)
    revision = (resource or {}).get('metadata', {}).get('resourceVersion', '')
    if not re.fullmatch(r'[0-9]{1,64}', revision):
        raise ValueError('Invalid publication credential resource version')
    return revision


def shadow_intake_config(kubectl, cluster_uid, repository, installer=None):
    api = installer or SimpleNamespace(owned_secret=owned_secret)
    current = api.owned_secret(kubectl, 'ai-sdlc-github-client', cluster_uid)
    if current is None:
        raise ValueError('Shadow intake requires an explicitly imported GitHub client; run credentials --github-auth gh-cli first')
    # Revalidate the existing credential without falling back to ambient gh.
    configure_github_client(kubectl, cluster_uid, repository, installer=installer)
    if api.owned_secret(kubectl, 'ai-sdlc-github-client', cluster_uid) != current:
        raise ValueError('GitHub client changed while deriving intake authorization; retry installation')
    credential = json.loads(current['credential.json'])
    policy = json.loads((LOCAL / 'intake-policy.json').read_text())
    if (set(policy) != {'version', 'baseBranch', 'triggerLabel', 'authorizedActorSource', 'pollSeconds', 'maxPages'}
            or policy.pop('authorizedActorSource') != 'github-client'):
        raise ValueError('Unsupported intake actor source')
    policy.update(repository=repository, authorizedActorIDs=[credential['actorID']])
    return {'configJSON': json.dumps(policy, sort_keys=True)}


def _main(argv, stack):
    selected = sys.argv[1:] if argv is None else argv
    if selected and selected[0] == 'runtime-upgrade':
        import runtime_upgrade_pause
        return runtime_upgrade_pause.main(selected[1:])
    if selected and selected[0] == 'upgrade-plan':
        import install_upgrade
        return install_upgrade.main(selected[1:])
    if selected and selected[0] == 'initial-runtime':
        import initial_activation
        return initial_activation.main(selected[1:])
    if selected and selected[0] == 'release-target':
        import release_target_install
        return release_target_install.main(selected[1:])
    if selected and selected[0] == 'prepare':
        import bootstrap_prepare
        return bootstrap_prepare.main(selected[1:])
    if selected and selected[0] == 'build-foundation':
        import build_foundation_install
        return build_foundation_install.main(selected[1:])
    if selected and selected[0] == 'registry-node':
        import registry_node_install
        return registry_node_install.main(selected[1:])
    if selected and selected[0] == 'supply-chain':
        import supply_chain_install
        return supply_chain_install.main(selected[1:])
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['plan', 'local', 'status', 'tools', 'host', 'credentials', 'sandbox', 'supply-chain', 'build-foundation', 'release-target', 'registry-node', 'initial-runtime'])
    parser.add_argument('--configure-host', action='store_true', help='Raise inotify limits to committed minima for this boot; requires root')
    parser.add_argument('--config', type=Path, help='Reviewed local installation JSON; credentials remain explicit CLI choices')
    parser.add_argument('--platform', choices=['kind', 'konflux', 'openshift'])
    parser.add_argument('--cluster')
    parser.add_argument('--kubeconfig', type=Path, help='Existing OpenShift cluster kubeconfig; --platform openshift only')
    parser.add_argument('--context', help='Context of --kubeconfig to install into; --platform openshift only')
    parser.add_argument('--provider', choices=['auto', 'docker', 'podman'])
    parser.add_argument('--images', type=Path)
    parser.add_argument('--build-bundle', type=Path, help='Reviewed public build profile, original slots and pinned controller image; Konflux only')
    parser.add_argument('--configure-prepared', action='store_true', help='Configure an empty prepared runtime while retaining both freezes and zero replicas')
    parser.add_argument('--provenance-trust', type=Path, help='Separately approved public provenance trust; prepared configuration only')
    parser.add_argument('--build-images', type=Path, help='Reviewed target controller image pins; prepared configuration only')
    parser.add_argument('--release-target', type=Path, help='Separately approved public Saferus target; prepared configuration only')
    parser.add_argument('--browser-profile', type=Path, help='Installed public browser scope bound to original node, namespace, CA, policies and immutable image')
    parser.add_argument('--release-images', type=Path, help='Reviewed pinned deployer publication; requires release target')
    parser.add_argument('--timeout', default='10m')
    parser.add_argument('--repository')
    parser.add_argument('--environment')
    parser.add_argument('--profile', choices=['maintenance', 'shadow', 'candidate', 'verification'])
    parser.add_argument('--merge', action='store_true', default=None, help='Enable low-risk reviewed-repository merge after authenticated required checks')
    parser.add_argument('--publication', action='store_true', default=None, help='Enable signed verified reviewed-repository PR publication; requires independent review')
    parser.add_argument('--independent-review', action='store_true', default=None, help='Enable isolated read-only review after behavior verification')
    parser.add_argument('--review-repair', action='store_true', default=None, help='Permit one repair using the original remaining generation budget; requires independent review')
    parser.add_argument('--github-auth', choices=['none', 'gh-cli'], default='none')
    parser.add_argument('--refresh-github-credential', action='store_true')
    parser.add_argument('--codex-auth-file', type=Path, help='Explicit local Codex authentication source for the candidate sidecar Secret')
    parser.add_argument('--refresh-model-credential', action='store_true')
    parser.add_argument('--model-upstream', type=Path, help='Reviewed OpenAI-compatible model service document (see docs/operations/model-gateway-adapters.md)')
    parser.add_argument('--runtime-class', help='Existing sandbox RuntimeClass for executor, reviewer and verification pods (for example kata); --platform openshift with the candidate or verification profile. kind installs and uses ai-sdlc-offline')
    parser.add_argument('--project-manifest', type=Path, help='Reviewed ai-sdlc.project/v1 manifest for a repository that is not a built-in reference')
    parser.add_argument('--flow-console', action='store_true', default=None, help='Enable the read-only flow console (docs/operations/flow-console.md); omitting it on a later install disables it')
    parser.add_argument('--dependency-proxy', help='Deploy the in-cluster dependency proxy for maven, npm or maven,npm (docs/operations/dependency-cache.md); omitting it on a later install removes it')
    parser.add_argument('--tekton-ci', action='store_true', default=None, help='Install pinned Tekton Pipelines and the reviewed Maven CI pipeline (docs/operations/tekton-ci.md); kind only')
    parser.add_argument('--tekton-ci-repository', action='append', help='Repository (owner/name) that gets its own Tekton CI Maven cache; repeatable; default: --repository')
    parser.add_argument('--tekton-ci-candidate-pods', type=int, help='Tekton CI candidate-code (build-test) pods that may run at once (1-32, default 1); checked against the node pid budget')
    parser.add_argument('--tekton-ci-fetch-pods', type=int, help='Tekton CI fetch pods that may run at once (1-32, default 1); checked against the node pid budget')
    parser.add_argument('--shared-storage', action='store_true', default=None, help='Create the kind cluster with one host directory shared by every node, served as the ReadWriteMany StorageClass ai-sdlc-shared for the Tekton CI caches and sealed dependency caches, so CI and cached verifications spread over the workers; requires --workers; set at creation (docs/operations/kind-node-resources.md)')
    parser.add_argument('--workers', type=int, help='Worker nodes of a new kind cluster (0-4, default 0: one node as before); sandboxed and CI pods then run only on the workers (docs/operations/kind-node-resources.md)')
    parser.add_argument('--node-pids-limit', type=int, help='Pid limit of each kind node container (default 8192, or a quarter of the host pid_max or threads-max when smaller; without the option a larger limit already set is kept); kind only (docs/operations/kind-node-resources.md)')
    parser.add_argument('--node-memory', help='Optional memory cap of each kind node that runs sandboxed pods (the single node, or each worker), for example 12g; off by default; kind only')
    parser.add_argument('--node-cpus', help='Optional CPU cap of each kind node that runs sandboxed pods (the single node, or each worker), for example 8 or 6.5; off by default; kind only')
    parser.add_argument('--console-images', type=Path, help='Reviewed console-images.json from tools/install_images.py --include-console; default: beside the image lock')
    parser.add_argument('--generation-seconds', type=int, help='Generation time budget in seconds (600-900, default 600); candidate or verification only, recorded in the execution configuration')
    parser.add_argument('--generation-steps', type=int, help='Generation SCC tool-step budget (24-32, default 24; above 24 requires --model-upstream); candidate or verification only, recorded in the execution configuration')
    parser.add_argument('--generation-answer-tokens', type=int, help='Requested output tokens per generator model answer (4096-8192, default 4096; above 4096 requires --model-upstream); candidate or verification only, recorded in the execution configuration')
    args = parser.parse_args(argv)
    if args.command not in ('plan','local','status','tools','host','credentials','sandbox'):
        raise ValueError('Delegated installer subcommands must be the first argument')
    config_digest = install_config.resolve(args)
    defaults = dict(platform='kind', cluster='ai-sdlc', provider='auto', images=LOCAL / 'images.lock.json',
                    repository='os-santiago/ai-sdlc', environment='ai-sdlc-production', profile='maintenance')
    for key, value in defaults.items():
        if getattr(args, key) is None:
            setattr(args, key, value)
    project_manifest = None
    if args.project_manifest is not None:
        # status, credentials and sandbox address the same installed scope.
        if args.command in ('plan', 'local') and args.profile != 'verification':
            raise ValueError('A project manifest requires the verification profile')
        project_manifest = project_install.load(args.project_manifest, args.repository)
        github_merge_policy.register_project(project_manifest)
    if args.build_bundle is not None and (args.command not in ('plan', 'local') or args.platform != 'konflux'
            or args.profile != 'verification' or args.repository != 'os-santiago/saferus'
            or args.environment != 'ai-sdlc-production' or not args.merge or not args.publication or not args.independent_review):
        raise ValueError('Build installation requires explicit Konflux Saferus verification/review/publication/merge scope')
    if args.configure_prepared or args.provenance_trust is not None or args.build_images is not None:
        if (not args.configure_prepared or args.command != 'local' or args.platform != 'konflux'
                or args.build_bundle is None or args.build_images is None or args.provenance_trust is None
                or args.refresh_github_credential or args.refresh_model_credential):
            raise ValueError('Prepared configuration requires explicit local Konflux build bundle, controller pins and approved provenance trust, without credential rotation')
    if args.release_target is not None or args.release_images is not None or args.browser_profile is not None:
        if not args.configure_prepared or args.release_target is None or args.release_images is None or args.browser_profile is None:
            raise ValueError('Release configuration requires prepared mode, approved target, published deployer pins and installed browser profile')
    konflux_runtime_install.validate_options(args)
    openshift_install.validate_options(args)
    tekton_ci_install.validate_options(args)
    node_resources = kind_nodes.validate_options(args)
    kind_nodes.validate_workers(args.workers)
    if args.workers is not None and (args.platform != 'kind' or args.command not in ('plan', 'local', 'sandbox')):
        raise ValueError('--workers requires plan, local or sandbox on the kind platform')
    if args.shared_storage and (args.platform != 'kind' or args.command not in ('plan', 'local')):
        raise ValueError('--shared-storage requires plan or local on the kind platform')
    if args.shared_storage and (args.workers == 0 or (args.command == 'plan' and not args.workers)):
        raise ValueError('--shared-storage needs workers')

    def capacity(workers, installed=None, sandboxed=None, limit=None, lowering=False, running=None, shared=None):
        # The nodes must hold their baseline, one verification run when
        # sandboxed execution is installed, and every CI pod the quotas admit.
        # Which quotas: when this run lowers a node limit, both the installed
        # and the requested ones (the installed ones stay until Tekton CI is
        # reapplied, or forever if --tekton-ci is omitted); otherwise the
        # requested ones, or the installed ones when --tekton-ci is omitted.
        shared = bool(args.shared_storage) if shared is None else shared
        quotas = tekton_ci_install.pod_quotas(args.tekton_ci_candidate_pods, args.tekton_ci_fetch_pods,
                                              workers, shared) if args.tekton_ci else None
        installed = installed or {'candidate': 0, 'fetch': 0}
        running = running or {}
        if lowering or quotas is None:
            counted = {k: max(installed[k], (quotas or {}).get(k, 0)) for k in ('candidate', 'fetch')}
        else:
            # Pods already running stay even when the quota is lowered.
            counted = {k: max(running.get(k, 0), quotas[k]) for k in ('candidate', 'fetch')}
        budget = kind_nodes.pid_budget(limit or node_resources['pidsLimit'],
                                       sandbox=args.profile in ('candidate', 'verification') if sandboxed is None else sandboxed,
                                       candidate_pods=counted['candidate'], fetch_pods=counted['fetch'], workers=workers,
                                       shared=shared)
        return quotas, budget
    tekton_quotas = tekton_ci_install.pod_quotas(args.tekton_ci_candidate_pods, args.tekton_ci_fetch_pods,
                                                 args.workers or 0, bool(args.shared_storage)) if args.tekton_ci else None
    node_budget = None
    if node_resources is not None and args.command == 'plan':
        # local checks before creating a cluster, and a reinstall checks
        # against the cluster's actual topology and installed quotas.
        tekton_quotas, node_budget = capacity(args.workers or 0)
    if args.runtime_class is not None:
        import candidate_install
        candidate_install.validate_runtime_class(args.runtime_class)
        if args.command not in ('plan', 'local') or args.profile not in ('candidate', 'verification'):
            raise ValueError('--runtime-class requires plan or local with the candidate or verification profile')
    runtime_class = args.runtime_class or 'ai-sdlc-offline'
    if args.merge and not args.publication:
        raise ValueError('Merge requires explicit verified publication')
    if args.merge is not None and args.command not in ('plan', 'local', 'status', 'credentials', 'sandbox'):
        raise ValueError('Merge option requires an installation scope')
    if args.merge and args.command == 'local' and args.github_auth != 'gh-cli':
        raise ValueError('Merge installation requires gh-cli authentication for native protection')
    if args.publication and (not project_install.reviewed(args.repository, project_manifest) or not args.independent_review or (args.command in ('plan', 'local') and args.profile != 'verification')):
        raise ValueError('Publication requires a reviewed repository, verification and independent review')
    if args.publication is not None and args.command not in ('plan', 'local', 'status', 'credentials', 'sandbox'):
        raise ValueError('Publication option requires an installation scope')
    if (args.flow_console is not None or args.console_images is not None) and args.command not in ('plan', 'local'):
        raise ValueError('--flow-console requires plan or local')
    dependency_proxy = None
    if args.dependency_proxy is not None:
        import dependency_proxy_install
        if args.command not in ('plan', 'local') or args.configure_prepared:
            # The prepared transition renders from its durable intent, which
            # does not record the proxy; refuse rather than silently omit it.
            raise ValueError('--dependency-proxy requires plan or local without --configure-prepared')
        dependency_proxy = dependency_proxy_install.values(dependency_proxy_install.parse(args.dependency_proxy))
    if args.console_images is not None and not args.flow_console:
        raise ValueError('--console-images requires --flow-console')
    if args.flow_console and args.platform == 'konflux':
        raise ValueError('--flow-console is not supported on the Konflux platform yet')
    if args.review_repair and not args.independent_review:
        raise ValueError('Review repair requires independent review')
    if args.review_repair is not None and args.command not in ('plan', 'local', 'status', 'credentials', 'sandbox'):
        raise ValueError('--review-repair requires an installation scope')
    if args.independent_review and args.command in ('plan', 'local') and args.profile != 'verification':
        raise ValueError('Independent review requires the verification profile')
    if args.independent_review is not None and args.command not in ('plan', 'local', 'status', 'credentials', 'sandbox'):
        raise ValueError('Independent review option requires an installation scope')
    if args.profile in ('shadow', 'candidate', 'verification') and args.command not in ('plan', 'local'):
        raise ValueError('--profile shadow/candidate/verification is valid only with plan or local')
    if (args.codex_auth_file is not None or args.refresh_model_credential) and (args.command != 'local' or args.profile not in ('candidate', 'verification')):
        raise ValueError('Model credentials are only accepted by an explicit local candidate installation')
    if args.generation_seconds is not None or args.generation_steps is not None:
        if args.command not in ('plan', 'local') or args.profile not in ('candidate', 'verification'):
            raise ValueError('A generation budget requires plan or local with the candidate or verification profile')
        import candidate_install
        candidate_install.generation_budget(args.generation_seconds, args.generation_steps, args.model_upstream is not None)
    if args.generation_answer_tokens is not None:
        if args.command not in ('plan', 'local') or args.profile not in ('candidate', 'verification'):
            raise ValueError('A generation answer-token bound requires plan or local with the candidate or verification profile')
        import candidate_install
        candidate_install.answer_tokens(args.generation_answer_tokens, args.model_upstream is not None)
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
    cache = tool_cache()
    if args.command != 'tools':
        image_lock(args.images)
    build_bundle = build_runtime_install.load_bundle(args.build_bundle, image_lock(args.images)) if args.build_bundle is not None and not args.configure_prepared else None
    binaries = tools(cache) if args.command == 'tools' else tools(cache, CLUSTER_TOOLS)
    if args.command == 'tools':
        print(json.dumps(binaries, indent=2))
        return 0
    helm_args = [binaries['helm'], 'template', 'ai-sdlc', CHART, '--namespace', NAMESPACE,
                 '-f', LOCAL / 'values.yaml', '-f', LOCAL / 'profiles' / (args.profile + '.json'), '-f', args.images]
    execution_values = None
    if args.profile in ('candidate', 'verification'):
        import candidate_install
        execution_values = candidate_install.desired_values(ROOT, args.images, image_lock(args.images), verification=args.profile == "verification", review=bool(args.independent_review), publication=bool(args.publication), merge=bool(args.merge), repair=bool(args.review_repair),
                                                          generation_seconds=args.generation_seconds, generation_steps=args.generation_steps,
                                                          model_upstream=args.model_upstream is not None, runtime_class=runtime_class,
                                                          generation_answer_tokens=args.generation_answer_tokens)
        if args.model_upstream is not None:
            candidate_install.apply_model_upstream(execution_values, args.model_upstream)
    elif args.model_upstream is not None:
        raise ValueError('A model upstream requires the candidate or verification profile')
    # A local install needs the reviewed console pin before any effect; a plan
    # renders without it when it is not there yet.
    console_digest = None
    if args.flow_console:
        pin = args.console_images or Path(args.images).with_name('console-images.json')
        # Only an absent default pin is tolerated, and only by plan; a pin
        # that exists but is invalid, or an explicit path, always fails.
        if args.command == 'local' or args.console_images is not None or pin.exists():
            console_digest = console_images(pin, image_lock(args.images))
    # The prepared transition renders the original complete state first and its
    # complete target after native policy reconciliation. Its partial selection
    # cannot satisfy the chart's mandatory installed merge-policy input yet.
    if not args.configure_prepared:
        with tempfile.NamedTemporaryFile(mode='w', suffix='.json') as preview:
            preview_values = copy.deepcopy(execution_values or {})
            if args.merge:
                # The native rule is reconciled only by local; the preview needs
                # a placeholder for the chart's mandatory installed policy.
                preview_values.setdefault('merge', {})['configJSON'] = json.dumps({'preview': True})
            if args.publication:
                preview_values['authority'] = {'policyJSON': json.dumps({'repository': args.repository})}
                if project_manifest is not None:
                    # The preview binds this manifest as the policy will after compilation.
                    preview_json = json.dumps(project_manifest, sort_keys=True, separators=(',', ':'))
                    preview_values['project'] = {'manifestJSON': preview_json}
                    preview_values['authority'] = {'policyJSON': json.dumps({'repository': args.repository,
                        'projectSHA256': hashlib.sha256(preview_json.encode()).hexdigest()})}
            if dependency_proxy is not None:
                preview_values['dependencyProxy'] = dependency_proxy
            if args.flow_console:
                preview_values['console'] = {'enabled': True}
                if console_digest is not None:
                    preview_values['console']['digest'] = console_digest
            json.dump(preview_values, preview); preview.flush()
            run(helm_args + ['-f', preview.name], stdout=subprocess.DEVNULL)
    tekton_plan = tekton_ci_install.plan() if args.tekton_ci else None
    if args.command == 'plan':
        print(json.dumps({'cluster': args.cluster, 'platform': args.platform, 'namespace': NAMESPACE,
                          'chart': str(CHART.relative_to(ROOT)), 'mode': args.profile,
                          'images': image_lock(args.images), 'deliveryEnabled': False,
                          'independentReviewEnabled': bool(args.independent_review),
                          'publicationEnabled': bool(args.publication),
                          'mergeEnabled': bool(args.merge),
                          'buildEnabled': build_bundle is not None,
                          # Only with the option: default output is unchanged.
                          **({'flowConsoleEnabled': True} if args.flow_console else {}),
                          **({'tektonCI': dict(tekton_plan, podQuotas=tekton_quotas)} if tekton_plan else {}),
                          **({'nodeResources': dict(node_resources, pidBudget=node_budget,
                                                    **({'workers': args.workers} if args.workers else {}))}
                             if node_resources else {}),
                          'githubAuth': args.github_auth, 'repository': args.repository,
                          'environment': args.environment, 'configurationDigest': config_digest}, indent=2))
        return 0
    state = Path(os.environ.get('XDG_STATE_HOME', Path.home() / '.local/state')) / 'ai-sdlc' / args.cluster
    state.mkdir(parents=True, exist_ok=True, mode=0o700)
    if args.platform != 'openshift':
        openshift_install.refuse_other_platform(state)
    provider = args.provider
    if args.platform == 'konflux':
        konflux_runtime_install.lock_state(state, stack)
        provider = konflux_runtime_install.select_provider(provider, state, args.cluster)
        if args.configure_prepared:
            import prepared_configuration
            return prepared_configuration.install_prepared(args,state,binaries,provider,execution_values)
        if args.command == 'local' and ((state / 'bootstrap-preparation.json').exists() or (state / 'bootstrap-preparation.json').is_symlink()):
            raise ValueError('Prepared runtime remains paused; use explicit --configure-prepared with reviewed inputs; never delete preparation custody for ordinary reconciliation')
    if provider == 'auto':
        provider = next((p for p in ('docker', 'podman') if shutil.which(p)), None)
    if not provider or not shutil.which(provider):
        raise ValueError('Install Docker or Podman, then rerun; the installer does not change host privileges.')
    run([provider, 'info'], stdout=subprocess.DEVNULL)
    openshift = args.platform == 'openshift'
    if args.command == 'local' and not openshift:
        host_prerequisites(args.configure_host)
    env = dict(os.environ, KIND_EXPERIMENTAL_PROVIDER=provider)
    kind = binaries['kind']
    if openshift:
        # An existing cluster: no kind lifecycle, node runtime or kind context.
        kubeconfig, kubectl, cluster_uid, created = openshift_install.connect(run, binaries, state, args)
        kube_context = args.context
    else:
        kubeconfig = state / 'kubeconfig'
        kube_context = 'kind-' + args.cluster
        clusters = run([kind, 'get', 'clusters'], env=env, capture_output=True).stdout.splitlines()
        marker = state / ('konflux-ownership.json' if args.platform == 'konflux' else 'ownership.json')
        if args.platform == 'konflux' and args.cluster not in clusters:
            raise ValueError('Recorded Konflux cluster is absent; refusing recreation')
        if args.cluster in clusters and not marker.exists():
            raise ValueError('Cluster already exists without this installer ownership record; choose a different --cluster.')
        if args.command in ('status', 'credentials', 'sandbox') and args.cluster not in clusters:
            raise ValueError('The requested local cluster does not exist')
        created = args.cluster not in clusters
        if created and args.shared_storage and not args.workers:
            # A new single-node cluster could never gain the mount or workers.
            raise ValueError('--shared-storage needs workers')
        if created and node_resources is not None and args.command == 'local':
            # Refuse an impossible budget before creating anything.
            tekton_quotas, node_budget = capacity(args.workers or 0)
        if created:
            config = LOCAL / 'kind.yaml'
            if args.workers:
                # kind.yaml plus the workers; written beside the kubeconfig.
                shared_dir = None
                if args.shared_storage:
                    import shared_storage
                    # Only the installing user can reach the shared caches on the host.
                    shared_dir = state / 'shared'
                    shared_storage.ensure_host_dir(shared_dir)
                config = state / 'kind-cluster.yaml'
                config.write_text(kind_nodes.cluster_config(args.workers, shared_dir))
            run([kind, 'create', 'cluster', '--name', args.cluster, '--config', config,
                 '--kubeconfig', kubeconfig, '--wait', args.timeout], env=env)
        run([kind, 'export', 'kubeconfig', '--name', args.cluster, '--kubeconfig', kubeconfig], env=env)
        kubeconfig.chmod(0o600)
        kubectl = [binaries['kubectl'], '--kubeconfig', kubeconfig, '--context', kube_context]
        cluster_uid = run(kubectl + ['get', 'namespace', 'kube-system', '-o', 'jsonpath={.metadata.uid}'], capture_output=True).stdout.strip()
        if created:
            if not cluster_uid:
                raise ValueError('Cannot establish the created cluster identity')
            marker.write_text(json.dumps({'cluster': args.cluster, 'provider': provider, 'clusterUID': cluster_uid}) + '\n')
        elif args.platform == 'konflux':
            konflux_runtime_install.verify_cluster(run, kubectl, state, args.cluster, provider, cluster_uid)
        else:
            check_ownership(marker, args.cluster, provider, cluster_uid)

    nodes = None

    def reconcile_nodes():
        nonlocal nodes, tekton_quotas, node_budget
        # kind cannot add or remove nodes: a reinstall keeps its topology.
        nodes = kind_nodes.topology(run, provider, kind, args.cluster, env)
        if args.workers is not None and len(nodes['workers']) != args.workers:
            raise ValueError('The cluster has %d workers; kind cannot change that, so use another --cluster'
                             % len(nodes['workers']))
        # Shared storage is a mount fixed at creation, like the workers.
        nodes['shared'] = bool(nodes['workers']) and kind_nodes.shared_mounted(
            run, provider, [nodes['controlPlane']] + nodes['workers'])
        if args.shared_storage and not nodes['shared']:
            raise ValueError('The cluster was created without --shared-storage; kind cannot add the mount, so use '
                             'another --cluster')
        # The budget covers both the quotas in the cluster and any requested
        # change, so it holds before, during and after this reconciliation,
        # and whether or not this run reapplies Tekton CI. Sandboxed pods are
        # counted when this run installs the offline runtime or it exists.
        installed = tekton_ci_install.installed_pod_quotas(run, kubectl)
        sandboxed = (args.command == 'sandbox' or args.profile in ('candidate', 'verification') or bool(run(
            kubectl + ['get', 'runtimeclass', 'ai-sdlc-offline', '--ignore-not-found', '-o', 'name'],
            capture_output=True).stdout.strip()))
        # A default keeps a larger limit already on a node: budget against
        # the smallest limit the nodes will actually have.
        names = [nodes['controlPlane']] + nodes['workers']
        limit = kind_nodes.effective_limit(run, provider, args.cluster, kind, env, node_resources, names)
        lowering = kind_nodes.lowers_limit(run, provider, names, node_resources)
        running = tekton_ci_install.installed_pod_quotas(run, kubectl, used_only=True)
        quotas, node_budget = capacity(len(nodes['workers']), installed, sandboxed, limit, lowering, running,
                                       nodes['shared'])
        if args.command == 'local':
            tekton_quotas = quotas
        for worker in nodes['workers']:
            current = kind_nodes.inspect(run, provider, worker)['pidsLimit']
            target = kind_nodes.target_limit(current, node_resources)
            if current is None or target < current:
                kind_nodes.admitted_pods_fit(run, kubectl, worker, target)
        # Set on creation and reconciled on every later install: podman's
        # default node pid limit (2048) is too small (#859).
        report = kind_nodes.apply_cluster(run, provider, args.cluster, kind, env, node_resources, nodes)
        if nodes['shared']:
            import shared_storage
            # Re-checked on every run: a loosened host directory is repaired.
            print(json.dumps({'sharedStorageHostDirectory': shared_storage.ensure_host_dir(state / 'shared')}),
                  file=sys.stderr)
            print(json.dumps({'sharedStorage': shared_storage.configure(run, kubectl, args.timeout)}), file=sys.stderr)
            kind_nodes.schedule(run, kubectl, nodes, shared=True)
        else:
            kind_nodes.schedule(run, kubectl, nodes, kind_nodes.ci_worker(run, kubectl, nodes))
        if nodes['workers']:
            import offline_runtime
            # The scheduler's per-worker pod cap follows each worker's limit
            # on every install, not only after ./install.sh sandbox.
            limits = {r['node']: r['pidsLimit'] for r in report}
            for worker in nodes['workers']:
                offline_runtime.reconcile_max_pods(kubectl, provider, args.cluster, worker, limits[worker])
        addresses = kind_nodes.node_addresses(run, provider, [nodes['controlPlane']] + nodes['workers'],
                                              required=bool(nodes['workers']))
        print(json.dumps({'nodeResources': report}), file=sys.stderr)
        # Pods leave through SNAT to their node's address; a host service that
        # allowlists clients (the local model service) must allow each one.
        print('Kind node addresses to allow in host services that allowlist clients: '
              + ', '.join(str(addresses[n]) + ' (' + n + ')' for n in sorted(addresses)), file=sys.stderr)
        return {'nodes': report, 'addresses': addresses}
    node_report = None
    if args.command == 'local' and execution_values is not None:
        candidate_install.require_runtime_class(kubectl, args.runtime_class)
        candidate_install.require_unchanged_tool_budget(kubectl, execution_values)
    if not created and args.platform == 'konflux':
        konflux_runtime_install.namespace(run, kubectl, state, cluster_uid, create=args.command == 'local')
        if args.command != 'status':
            konflux_runtime_install.require_frozen(run, kubectl)
    if args.command == 'status':
        run(kubectl + ['get', 'deployments,statefulsets,pods,pvc', '-n', NAMESPACE])
        config = read_resource(kubectl, 'configmap', 'ai-sdlc-config')
        mode = (config or {}).get('data', {}).get('AI_SDLC_RUNTIME_MODE', 'unknown')
        print('Installation mode: ' + mode + '. Autonomous issue-to-production delivery: unavailable.')
        if args.tekton_ci:
            observation = tekton_ci_install.observe(run, kubectl)
            print(json.dumps({'tektonCI': observation}, indent=2))
            return 0 if observation['ready'] else 1
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
        if node_resources is not None:
            reconcile_nodes()
        lock = image_lock(args.images)
        result = offline_runtime.install(kubectl, provider, kind, args.cluster, cluster_uid,
                                         cache / 'offline-runtime',
                                         lock['image']['repository'] + '@' + lock['components']['worker']['digest'])
        (state / 'offline-runtime.json').write_text(json.dumps(result, indent=2) + '\n')
        print(json.dumps(result, indent=2))
        return 0
    # kind's node readiness alone does not establish service networking or
    # dynamic storage. Fail here before consuming the bounded migration window.
    if openshift:
        openshift_install.preflight(run, kubectl, sandbox=args.profile in ('candidate', 'verification'), runtime_class=runtime_class)
        if execution_values is not None:
            # Quota headroom for the class's pod overhead (Kata declares one).
            overhead = openshift_install.pod_overhead(run, kubectl, runtime_class)
            if overhead:
                execution_values.setdefault('sandbox', {'runtimeClassName': runtime_class})['podOverhead'] = overhead
    else:
        run(kubectl + ['wait', '--for=condition=Ready', 'pods', '--all', '-n', 'kube-system',
                       '--timeout', args.timeout])
        run(kubectl + ['rollout', 'status', 'deployment/local-path-provisioner',
                       '-n', 'local-path-storage', '--timeout', args.timeout])
        if node_resources is not None:
            node_report = reconcile_nodes()
    if args.platform != 'konflux':
        run(kubectl + ['apply', '-f', LOCAL / 'namespace.yaml'])
    if build_bundle is not None:
        build_runtime_install.preflight(run, kubectl, provider, args.cluster, image_lock(args.images), build_bundle)
    if args.github_auth == 'gh-cli':
        configure_github_client(kubectl, cluster_uid, args.repository, args.refresh_github_credential)
    intake_values = None
    if args.profile in ('shadow', 'candidate', 'verification'):
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
    if args.merge:
        import github_merge_install
        native_policy = github_merge_install.reconcile(state, args.repository)
        if project_manifest is not None:
            project_install.require_ruleset(project_manifest, native_policy)
        execution_values['merge']['configJSON'] = json.dumps(native_policy, sort_keys=True)
    public_values = configure_identities(kubectl, provider, image_lock(args.images),
                                         cluster_uid, args.repository, args.environment,
                                         freeze=args.profile == 'maintenance', candidate=args.profile in ('candidate', 'verification'), verification=args.profile == 'verification', publication=bool(args.publication), merge=bool(args.merge), build=build_bundle is not None, review=bool(args.independent_review),
                                         project_manifest=project_manifest, flow_console=bool(args.flow_console))
    if dependency_proxy is not None:
        public_values['dependencyProxy'] = dependency_proxy
    if args.flow_console:
        # Values name the image only; the credential stays in its Secret.
        public_values['console'] = {'enabled': True, 'digest': console_digest}
    if intake_values is not None:
        public_values['intake'] = intake_values
    if args.profile in ('candidate', 'verification'):
        import candidate_install
        import offline_runtime
        lock = image_lock(args.images)
        execution_values["candidate"]["credentialRevision"] = candidate_install.model_credentials(kubectl, cluster_uid, args.codex_auth_file, args.refresh_model_credential)
        if args.independent_review:
            candidate_install.review_credentials(kubectl, cluster_uid)
        # Verification and review read source with the installation's
        # repository-scoped read credential when the administrator supplied
        # one; otherwise they stay anonymous (rendering unchanged).
        if args.profile == 'verification':
            candidate_install.apply_source_credential(execution_values, kubectl, cluster_uid, args.repository)
        if args.publication:
            execution_values['publication']['credentialRevision'] = configure_publication_client(kubectl, cluster_uid, args.repository, args.github_auth, args.refresh_github_credential, project_manifest=project_manifest)
        if not openshift:
            offline_runtime.install(kubectl, provider, kind, args.cluster, cluster_uid, cache,
                                    lock['image']['repository'] + '@' + lock['components']['worker']['digest'])
        execution_values['candidate']['apiServerCIDRs'] = candidate_install.api_cidrs(kubectl)
        public_values.update(execution_values)
    if build_bundle is not None:
        public_values['build'] = build_runtime_install.values(provider, image_lock(args.images), build_bundle, public_values, intake_values)
    # This temporary Helm input contains public verification keys only. Helm
    # owns workloads and the policy ConfigMap; private data stays in Secrets.
    with tempfile.NamedTemporaryFile(mode='w', suffix='.json') as values:
        json.dump(public_values, values)
        values.flush()
        run([binaries['helm'], 'upgrade', '--install', 'ai-sdlc', CHART,
             '--kubeconfig', kubeconfig, '--kube-context', kube_context,
             '--namespace', NAMESPACE, '-f', LOCAL / 'values.yaml',
             '-f', LOCAL / 'profiles' / (args.profile + '.json'), '-f', args.images,
             # Poll readiness during recovery rather than failing immediately
             # on a Deployment's stale ProgressDeadlineExceeded condition.
             '-f', values.name, *(['--set', 'openshift.enabled=true'] if openshift else []),
             '--wait=legacy', '--wait-for-jobs', '--timeout', args.timeout])
    run(kubectl + ['rollout', 'status', 'statefulset/ai-sdlc-postgres', '-n', NAMESPACE,
                   '--timeout', args.timeout])
    for name in list(COMPONENT_NAMES.values()) + (["verifier"] if args.profile == "verification" else []) + (["build-controller"] if build_bundle is not None else []):
        run(kubectl + ['rollout', 'status', 'deployment/ai-sdlc-' + name, '-n', NAMESPACE,
                       '--timeout', args.timeout])
    if args.flow_console:
        run(kubectl + ['rollout', 'status', 'deployment/' + CONSOLE_SERVICE, '-n', NAMESPACE,
                       '--timeout', args.timeout])
    else:
        # The migrator has run without the console entry: drop its URL Secret.
        remove_console_credential(kubectl, cluster_uid)
    lock = image_lock(args.images)
    network = network_probe.verify(kubectl, lock['image']['repository'] + '@' + lock['components']['worker']['digest'])
    worker_network = None
    if nodes and nodes['workers']:
        # The same conformance on the workers, where sandboxed pods run.
        worker_network = network_probe.verify(kubectl, lock['image']['repository'] + '@' + lock['components']['worker']['digest'],
                                              placement={'nodeSelector': {kind_nodes.ROLE_LABEL: kind_nodes.ROLE_SANDBOX},
                                                         'tolerations': [kind_nodes.toleration()]},
                                              # A fresh worker first pulls the probe image.
                                              ready_seconds=300)
    # After the runtime is ready: a Tekton failure leaves the runtime intact
    # and the owned Tekton state in place for reapplication.
    tekton_ci = tekton_ci_install.install(run, kubectl, cluster_uid, tekton_ci_install.cache_dir(), args.timeout,
                                          args.tekton_ci_repository or [args.repository.lower()],
                                          tekton_quotas,
                                          kind_nodes.tekton_pod_template(nodes['shared']) if nodes and nodes['workers'] else None,
                                          kind_nodes.SHARED_CLASS if nodes and nodes.get('shared') else None) if args.tekton_ci else None
    report = {'cluster': args.cluster, 'platform': args.platform, 'namespace': NAMESPACE, 'provider': provider,
              'repository': args.repository, 'environment': args.environment,
              'configurationDigest': config_digest,
              'projectSHA256': json.loads(public_values['authority']['policyJSON']).get('projectSHA256', ''),
              'mode': args.profile, 'installationReady': True, 'deliveryEnabled': False,
              'issueIntakeEnabled': args.profile in ('shadow', 'candidate', 'verification'),
              'candidateExecutionEnabled': args.profile in ('candidate', 'verification'),
              'independentVerificationEnabled': args.profile == 'verification',
              'independentReviewEnabled': bool(args.independent_review),
                          'publicationEnabled': bool(args.publication),
                          'mergeEnabled': bool(args.merge),
                          'buildEnabled': build_bundle is not None,
                          # Only with the option: default output is unchanged.
                          **({'flowConsoleEnabled': True} if args.flow_console else {}),
              'networkPolicyVerification': network,
              **({'workerNetworkPolicyVerification': worker_network} if worker_network else {}),
              **({'tektonCI': tekton_ci} if tekton_ci else {}),
              **({'nodeResources': {'nodes': node_report['nodes'], 'pidBudget': node_budget,
                                    'controlPlane': nodes['controlPlane'], 'workers': nodes['workers'],
                                    **({'sharedStorage': True} if nodes.get('shared') else {}),
                                    'nodeAddresses': node_report['addresses']}} if node_report else {}),
              'images': image_lock(args.images)}
    (state / 'installation.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report, indent=2))
    if args.flow_console:
        # One line, no credential: the Service is ClusterIP and the policy
        # admits no peer by default, so port-forward is the access path.
        print('Flow console: kubectl --kubeconfig ' + str(kubeconfig) + ' --context ' + str(kube_context)
              + ' -n ' + NAMESPACE + ' port-forward svc/' + CONSOLE_SERVICE + ' 8080:8091  (then open http://127.0.0.1:8080/)', file=sys.stderr)
    return 0


def main(argv=None):
    with ExitStack() as stack:
        return _main(argv, stack)


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        # Do not serialize process input (which can contain a Secret).
        print('Installation failed: ' + str(error), file=sys.stderr)
        sys.exit(1)
