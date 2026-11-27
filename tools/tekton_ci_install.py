#!/usr/bin/env python3
"""Install pinned Tekton Pipelines and the reviewed Maven CI into an owned kind cluster.

Opt-in through `./install.sh local --tekton-ci` (docs/operations/tekton-ci.md).
Desired state lives under runtime/k8s/deploy/tekton-ci/. The upstream release
manifest is downloaded once, bounded and verified against its recorded SHA-256
and image digests before anything is applied. Every applied object carries the
installation's cluster UID; an existing Tekton installation without that mark
is never adopted. Re-application is idempotent server-side apply under one
field manager that never forces ownership conflicts. Omitting the option later
does not uninstall anything: deleting Tekton's CRDs deletes every run, so
removal is a deliberate operator step.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
DESIRED = ROOT / 'runtime/k8s/deploy/tekton-ci'
CI_DIR = DESIRED / 'ci'
CI_FILES = ('namespace.yaml', 'serviceaccount.yaml', 'admission-policy.yaml', 'network-policy.yaml',
            'task-source-checkout.yaml', 'task-apply-candidate.yaml', 'task-maven-build-test.yaml', 'task-maven-dependencies.yaml',
            'task-npm-lock-update.yaml', 'pipeline-maven-build-test.yaml', 'pipeline-maven-dependencies.yaml',
            'pipeline-npm-lock-update.yaml')
TASKS = ('source-checkout', 'apply-candidate', 'maven-build-test', 'maven-dependencies', 'npm-lock-update')
# The reviewed catalog pipelines.
PIPELINES = ('ai-sdlc-maven-build-test', 'ai-sdlc-maven-dependencies', 'ai-sdlc-npm-lock-update')
CI_NAMESPACE = 'ai-sdlc-ci'
FIELD_MANAGER = 'ai-sdlc-tekton-ci'
OWNER_ANNOTATION = 'ai-sdlc.io/cluster-uid'
OWNER_LABEL = 'ai-sdlc.io/tekton-ci'
IMAGE_LINE = re.compile(r'^\s*(?:-\s*)?image:\s*(\S+)\s*$', re.M)
PINNED_IMAGE = re.compile(r'[a-z0-9.-]+(?::[0-9]+)?/[a-z0-9._/-]+(?::[A-Za-z0-9._-]+)?@sha256:[a-f0-9]{64}')
ADMISSION_ATTEMPTS = 30
# Cluster-scoped CI objects (the admission policy) carry this name prefix.
CI_CLUSTER_PREFIX = 'ai-sdlc-ci-'
CACHE_PREFIX = 'ai-sdlc-ci-maven-cache-'
CACHE_SIZE = '8Gi'
REPOSITORY = re.compile(r'[a-z0-9][a-z0-9._-]{0,99}/[a-z0-9][a-z0-9._-]{0,99}')


def cache_claim(repository):
    """One Maven cache claim per repository, named from its digest.

    A run can bind only the claim derived from its own repository, and only
    claims the installer created exist, so one repository's dependencies never
    serve another's build. tools/tekton_ci_run.py and internal/tektontasks
    derive the same name."""
    if not REPOSITORY.fullmatch(repository) or any(p in ('.', '..') for p in repository.split('/')):
        raise ValueError('Tekton CI cache repositories must be lowercase owner/name')
    return CACHE_PREFIX + hashlib.sha256(repository.encode()).hexdigest()[:16]


def cache_claims(repositories):
    return [{'apiVersion': 'v1', 'kind': 'PersistentVolumeClaim',
             'metadata': {'name': cache_claim(r), 'namespace': CI_NAMESPACE,
                          'labels': {'app.kubernetes.io/part-of': 'ai-sdlc'},
                          'annotations': {'ai-sdlc.io/repository': r}},
             'spec': {'accessModes': ['ReadWriteOnce'], 'resources': {'requests': {'storage': CACHE_SIZE}}}}
            for r in sorted(set(repositories))]


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def lockfile():
    raw = (DESIRED / 'upstream.lock.json').read_bytes()
    lock = json.loads(raw)
    release = lock.get('release', '')
    keys = {'version', 'release', 'url', 'sha256', 'bytes', 'maxBytes', 'images', 'namespaces',
            'customResourceDefinitions', 'requiredDeployments'}
    if (set(lock) != keys or lock['version'] != 1
            or not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+', release)
            or lock['url'] != 'https://github.com/tektoncd/pipeline/releases/download/' + release + '/release.yaml'
            or not re.fullmatch(r'[a-f0-9]{64}', lock['sha256'])
            or type(lock['maxBytes']) is not int or not 0 < lock['maxBytes'] <= 8 * 1024 * 1024
            or type(lock['bytes']) is not int or not 0 < lock['bytes'] <= lock['maxBytes']):
        raise ValueError('Unsupported Tekton release lock')
    image = re.compile(r'ghcr\.io/tektoncd/pipeline/[a-z]+-[a-f0-9]{32}:' + re.escape(release) + r'@sha256:[a-f0-9]{64}')
    if not lock['images'] or lock['images'] != sorted(set(lock['images'])) or not all(image.fullmatch(i) for i in lock['images']):
        raise ValueError('Tekton release images must be unique, sorted and pinned by digest')
    for key, pattern in (('namespaces', r'tekton-[a-z-]+'),
                         ('customResourceDefinitions', r'[a-z]+\.(?:resolution\.)?tekton\.dev'),
                         ('requiredDeployments', r'tekton-[a-z-]+/tekton-[a-z-]+')):
        values = lock[key]
        if not values or values != sorted(set(values)) or not all(re.fullmatch(pattern, v) for v in values):
            raise ValueError('Invalid Tekton inventory: ' + key)
    if {d.split('/')[0] for d in lock['requiredDeployments']} - set(lock['namespaces']):
        raise ValueError('Required Tekton deployments must live in owned namespaces')
    return lock, digest(raw)


def configuration():
    raw = (DESIRED / 'configuration.json').read_bytes()
    config = json.loads(raw)
    maps = config.get('configMaps')
    if set(config) != {'version', 'configMaps'} or config['version'] != 1 or not isinstance(maps, dict) or not maps:
        raise ValueError('Unsupported Tekton configuration')
    for name, data in maps.items():
        if (not re.fullmatch(r'tekton-[a-z-]+/[a-z-]+', name) or not isinstance(data, dict) or not data
                or not all(isinstance(k, str) and isinstance(v, str) for k, v in data.items())):
            raise ValueError('Invalid Tekton configuration entry: ' + name)
    flags = maps.get('tekton-pipelines/feature-flags', {})
    # Restricted Pod Security and the read-only root filesystem for Tekton's
    # injected containers, and only installed definitions (no inline specs).
    for key in ('set-security-context', 'set-security-context-read-only-root-filesystem'):
        if flags.get(key) != 'true':
            raise ValueError('Tekton configuration must harden injected containers: ' + key)
    if set(flags.get('disable-inline-spec', '').split(',')) != {'pipeline', 'pipelinerun', 'taskrun'}:
        raise ValueError('Tekton configuration must refuse inline pipeline and task specifications')
    return config, digest(raw)


def ci_manifests():
    """Return the reviewed CI manifest bytes after checking every image is pinned."""
    if sorted(p.name for p in CI_DIR.iterdir()) != sorted(CI_FILES):
        raise ValueError('Unexpected Tekton CI desired-state files')
    raw = {name: (CI_DIR / name).read_bytes() for name in CI_FILES}
    for name, data in raw.items():
        for image in IMAGE_LINE.findall(data.decode('utf-8')):
            if not PINNED_IMAGE.fullmatch(image):
                raise ValueError('Tekton CI image must be pinned by digest: ' + name)
    return raw, digest(b''.join(name.encode() + b'\0' + raw[name] for name in CI_FILES))


def verify_release(raw, lock):
    if len(raw) != lock['bytes'] or digest(raw) != lock['sha256']:
        raise ValueError('Tekton release integrity check failed')
    text = raw.decode('utf-8')
    found = sorted(set(re.findall(r'ghcr\.io/tektoncd/[^\s"\',]+', text)))
    if found != lock['images']:
        raise ValueError('Tekton release image references differ from the lock')
    for image in IMAGE_LINE.findall(text):
        if not PINNED_IMAGE.fullmatch(image.strip('"\'')):
            raise ValueError('Tekton release container image is not pinned by digest')
    return text


def fetch_release(cache, lock, opener=urllib.request.urlopen):
    """Return verified release bytes from the cache or one bounded download."""
    cache.mkdir(parents=True, exist_ok=True)
    target = cache / ('tekton-pipeline-' + lock['release'] + '.yaml')
    if target.exists():
        raw = target.read_bytes()
        if len(raw) == lock['bytes'] and digest(raw) == lock['sha256']:
            return raw
    with opener(lock['url'], timeout=120) as response:
        raw = response.read(lock['maxBytes'] + 1)
    if len(raw) > lock['maxBytes']:
        raise ValueError('Tekton release exceeds its size bound')
    verify_release(raw, lock)
    with tempfile.NamedTemporaryFile(dir=cache, delete=False) as tmp:
        tmp.write(raw)
        temporary = Path(tmp.name)
    temporary.replace(target)
    return raw


def objects(run, kubectl, raw):
    """Convert manifests to JSON with kubectl's own parser (the installer is stdlib-only)."""
    result = run(kubectl + ['create', '--dry-run=client', '-o', 'json', '-f', '-'],
                 input=raw if isinstance(raw, str) else raw.decode('utf-8'), capture_output=True)
    # kubectl prints one JSON document per object, concatenated.
    decoder, text, position, items = json.JSONDecoder(), result.stdout, 0, []
    while True:
        while position < len(text) and text[position].isspace():
            position += 1
        if position == len(text):
            break
        document, position = decoder.raw_decode(text, position)
        items.extend(document['items'] if isinstance(document, dict) and document.get('kind') == 'List' else [document])
    if not items or any(not isinstance(i, dict) or not i.get('kind') or not i.get('metadata', {}).get('name') for i in items):
        raise ValueError('Unexpected manifest conversion result')
    return items


def key(item):
    meta = item['metadata']
    return item['kind'], meta.get('namespace', ''), meta['name']


def own(items, cluster_uid):
    for item in items:
        meta = item['metadata']
        meta.setdefault('annotations', {})[OWNER_ANNOTATION] = cluster_uid
        meta.setdefault('labels', {})[OWNER_LABEL] = 'owned'
    return items


def desired_tekton(items, lock, config, cluster_uid):
    names = {key(i) for i in items}
    expected = ({('Namespace', '', n) for n in lock['namespaces']}
                | {('CustomResourceDefinition', '', n) for n in lock['customResourceDefinitions']}
                | {('Deployment',) + tuple(d.split('/')) for d in lock['requiredDeployments']})
    if not expected <= names:
        raise ValueError('Tekton release lacks the locked inventory')
    for item in items:
        kind, namespace, name = key(item)
        if kind == 'Namespace' and name not in lock['namespaces']:
            raise ValueError('Tekton release creates an unlocked namespace')
        if namespace and namespace not in lock['namespaces']:
            raise ValueError('Tekton release writes outside its locked namespaces')
        if kind == 'Deployment':
            pod = item['spec']['template']['spec']
            for container in pod.get('initContainers', []) + pod.get('containers', []):
                if container.get('image') not in lock['images']:
                    raise ValueError('Tekton deployment image differs from the lock')
    by_name = {item['metadata'].get('namespace', '') + '/' + item['metadata']['name']: item
               for item in items if item['kind'] == 'ConfigMap'}
    for name, overrides in config['configMaps'].items():
        target = by_name.get(name)
        if target is None:
            raise ValueError('Tekton configuration targets an unknown ConfigMap: ' + name)
        data = target.setdefault('data', {})
        unknown = set(overrides) - set(data)
        if unknown:
            raise ValueError('Tekton configuration sets unknown keys: ' + ', '.join(sorted(unknown)))
        data.update(overrides)
    return own(items, cluster_uid)


def desired_ci(items, cluster_uid):
    for item in items:
        kind, namespace, name = key(item)
        cluster_scoped = kind in ('ValidatingAdmissionPolicy', 'ValidatingAdmissionPolicyBinding')
        if cluster_scoped and (namespace or not name.startswith(CI_CLUSTER_PREFIX)):
            raise ValueError('Cluster-scoped Tekton CI objects must be named ' + CI_CLUSTER_PREFIX + '*')
        if not cluster_scoped and ((kind == 'Namespace' and name != CI_NAMESPACE) or (kind != 'Namespace' and namespace != CI_NAMESPACE)):
            raise ValueError('Tekton CI resources must stay in ' + CI_NAMESPACE)
        if kind in ('Role', 'RoleBinding', 'ClusterRole', 'ClusterRoleBinding', 'Secret'):
            raise ValueError('Tekton CI desired state must not grant API access or carry Secrets')
        if kind == 'ServiceAccount' and item.get('automountServiceAccountToken') is not False:
            raise ValueError('The CI service account must not mount an API token')
    return own(items, cluster_uid)


def require_ownership(run, kubectl, items, cluster_uid):
    """Refuse to adopt any existing namespace or cluster-scoped object this installation did not create.

    This covers the Tekton and CI namespaces, CRDs, ClusterRoles and their
    bindings, webhook configurations and the CI admission policy: every object
    that is not inside a namespace this check has already proven to be ours."""
    roots = [(i['kind'].lower() + '.' + i['apiVersion'].split('/')[0] if '/' in i['apiVersion'] else i['kind'].lower(),
              i['metadata']['name']) for i in items if not i['metadata'].get('namespace')]
    for kind, name in roots:
        result = run(kubectl + ['get', kind, name, '--ignore-not-found', '-o', 'json'], capture_output=True)
        if not result.stdout.strip():
            continue
        annotations = json.loads(result.stdout).get('metadata', {}).get('annotations') or {}
        if annotations.get(OWNER_ANNOTATION) != cluster_uid:
            raise ValueError('Existing ' + kind + ' ' + name + ' is not owned by this installation; refusing to adopt it')


def apply(run, kubectl, items, dry_run=False):
    document = json.dumps({'apiVersion': 'v1', 'kind': 'List', 'items': items})
    return run(kubectl + ['apply', '--server-side', '--field-manager', FIELD_MANAGER,
                          *(['--dry-run=server'] if dry_run else []), '-f', '-'],
               input=document, capture_output=True, check=not dry_run)


def wait_ready(run, kubectl, lock, timeout):
    run(kubectl + ['wait', '--for=condition=Established', '--timeout', timeout]
        + ['customresourcedefinition/' + n for n in lock['customResourceDefinitions']], stdout=subprocess.DEVNULL)
    for name in lock['requiredDeployments']:
        namespace, deployment = name.split('/')
        run(kubectl + ['rollout', 'status', 'deployment/' + deployment, '-n', namespace, '--timeout', timeout],
            stdout=subprocess.DEVNULL)


def wait_admission(run, kubectl, items, sleep=time.sleep):
    """Tekton's webhook may serve before its certificate is trusted: retry a server dry run."""
    detail = ''
    for attempt in range(ADMISSION_ATTEMPTS):
        result = apply(run, kubectl, items, dry_run=True)
        if result.returncode == 0:
            return attempt + 1
        # The reviewed CI objects carry no credential; keep one bounded line.
        lines = (getattr(result, 'stderr', '') or '').strip().splitlines()
        detail = lines[-1][:300] if lines else ''
        sleep(4)
    raise ValueError('Tekton admission did not accept the reviewed CI definitions' + (': ' + detail if detail else ''))


def observe(run, kubectl, lock=None):
    """Fresh, read-only readiness of the owned Tekton installation and CI definitions."""
    lock = lock or lockfile()[0]
    deployments = {}
    for name in lock['requiredDeployments']:
        namespace, deployment = name.split('/')
        result = run(kubectl + ['get', 'deployment', deployment, '-n', namespace, '--ignore-not-found', '-o', 'json'],
                     capture_output=True)
        item = json.loads(result.stdout) if result.stdout.strip() else None
        status, spec = (item or {}).get('status', {}), (item or {}).get('spec', {})
        desired = spec.get('replicas', 0)
        deployments[name] = bool(item and desired > 0
                                 and status.get('observedGeneration') == item['metadata'].get('generation')
                                 and status.get('updatedReplicas') == desired
                                 and status.get('readyReplicas') == desired
                                 and status.get('availableReplicas') == desired)
    flags = run(kubectl + ['get', 'configmap', 'feature-flags', '-n', 'tekton-pipelines', '--ignore-not-found',
                           '-o', 'json'], capture_output=True)
    data = (json.loads(flags.stdout) if flags.stdout.strip() else {}).get('data') or {}
    hardened = all(data.get(k) == v for k, v in configuration()[0]['configMaps']['tekton-pipelines/feature-flags'].items())
    listed = run(kubectl + ['get', 'pipeline.tekton.dev', '-n', CI_NAMESPACE, '-o', 'name'],
                 capture_output=True, check=False)
    names = {line.rsplit('/', 1)[-1] for line in listed.stdout.split()} if listed.returncode == 0 else set()
    present = sorted(set(PIPELINES) & names)
    tasks = run(kubectl + ['get', 'task.tekton.dev', '-n', CI_NAMESPACE, '-o', 'name'], capture_output=True, check=False)
    task_names = {line.rsplit('/', 1)[-1] for line in tasks.stdout.split()} if tasks.returncode == 0 else set()
    present_tasks = sorted(set(TASKS) & task_names)
    return {'release': lock['release'], 'deployments': deployments, 'hardenedConfiguration': hardened,
            'pipelines': present, 'tasks': present_tasks,
            'ready': all(deployments.values()) and hardened and present == sorted(PIPELINES) and present_tasks == sorted(TASKS)}


def plan():
    """Offline validation of the desired state; no download or cluster access."""
    lock, lock_digest = lockfile()
    _, config_digest = configuration()
    _, ci_digest = ci_manifests()
    return {'release': lock['release'], 'lockSHA256': lock_digest, 'configurationSHA256': config_digest,
            'ciSHA256': ci_digest, 'namespace': CI_NAMESPACE, 'pipelines': list(PIPELINES)}


def install(run, kubectl, cluster_uid, cache, timeout, repositories):
    lock, lock_digest = lockfile()
    config, config_digest = configuration()
    ci_raw, ci_digest = ci_manifests()
    claims = cache_claims(repositories)
    raw = fetch_release(cache, lock)
    verify_release(raw, lock)
    tekton = desired_tekton(objects(run, kubectl, raw), lock, config, cluster_uid)
    require_ownership(run, kubectl, tekton + [{'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': CI_NAMESPACE}}], cluster_uid)
    apply(run, kubectl, tekton)
    wait_ready(run, kubectl, lock, timeout)
    # Tekton kinds are only known to kubectl once the CRDs are established.
    ci = desired_ci(objects(run, kubectl, b'\n---\n'.join(ci_raw[name] for name in CI_FILES)) + claims, cluster_uid)
    require_ownership(run, kubectl, ci, cluster_uid)
    # A server dry run cannot create the namespace its other objects need.
    apply(run, kubectl, [i for i in ci if i['kind'] == 'Namespace'])
    namespaced = [i for i in ci if i['kind'] != 'Namespace']
    attempts = wait_admission(run, kubectl, namespaced)
    apply(run, kubectl, namespaced)
    observation = observe(run, kubectl, lock)
    if not observation['ready']:
        raise ValueError('Tekton CI is not ready after installation; owned state is retained for reapplication')
    return dict(observation, lockSHA256=lock_digest, configurationSHA256=config_digest, ciSHA256=ci_digest,
                admissionAttempts=attempts, cacheClaims={r: cache_claim(r) for r in sorted(set(repositories))})


def validate_options(args):
    if not getattr(args, 'tekton_ci', None):
        if getattr(args, 'tekton_ci_repository', None):
            raise ValueError('--tekton-ci-repository requires --tekton-ci')
        return
    if args.command not in ('plan', 'local', 'status'):
        raise ValueError('--tekton-ci requires plan, local or status')
    if args.platform != 'kind':
        raise ValueError('--tekton-ci supports the kind platform only; Konflux and OpenShift provide their own Tekton')
    for repository in getattr(args, 'tekton_ci_repository', None) or []:
        cache_claim(repository)


def cache_dir():
    return Path(os.environ.get('XDG_CACHE_HOME', Path.home() / '.cache')) / 'ai-sdlc/tekton'
