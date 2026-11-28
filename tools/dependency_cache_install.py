"""Seal a dependency cache into the verifier namespace (verification.dependencies).

Verification pods never get a network. For a manifest that declares
``verification.dependencies`` the runtime mounts, read-only, the sealed cache
claim ``deps-<key>`` in the verifier namespace, where ``key`` covers the image,
the dependencies section and the base tree's dependency-defining files
(``project.Verification.DependencyKey``).

The cache itself is filled by the reviewed ``ai-sdlc-maven-dependencies``
Tekton Task, on the base revision only, with the dependency proxy as its only
reachable host. This operator tool brackets that run:

``key``
    print the claim name for a base revision, computed from ``git archive`` of
    that revision in a local clone (the same bytes as the forge archive the
    runtime reads; never a working tree);
``prepare``
    create the empty work claim ``deps-<key>`` in the CI namespace, to bind as
    the Task's writable ``maven-cache`` workspace;
``seal``
    once no pod uses the work claim any more, hand its volume over to the
    verifier namespace (the standard reclaim-policy and claimRef rebind) as
    ``deps-<key>``, labelled sealed from creation. Nothing that ran with
    network access can write to it afterwards.

``seal`` patches a PersistentVolume, so it needs a cluster-admin kubeconfig.
"""
import argparse
import fnmatch
import hashlib
import io
import json
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import time

KEY_VERSION = 'ai-sdlc.dependencies/v1'
CI_NAMESPACE = 'ai-sdlc-ci'
VERIFIER_NAMESPACE = 'ai-sdlc-verifiers'
KEY_ANNOTATION = 'ai-sdlc.io/dependency-key'
STATE_LABEL = 'ai-sdlc.io/dependency-state'
# Struct order of project.Dependencies: the key hashes the same canonical JSON.
FIELDS = ('cachePath', 'keyGlobs')
PINNED_IMAGE = re.compile(r'[a-z0-9.-]+(:[0-9]{1,5})?(/[a-z0-9._-]+)+@sha256:[a-f0-9]{64}')
KEY = re.compile(r'[a-f0-9]{64}')
REVISION = re.compile(r'[a-f0-9]{40}')
NAMESPACE = re.compile(r'[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?')
SIZE = re.compile(r'[1-9][0-9]{0,3}Gi')
MAX_ARCHIVE_BYTES = 1024 * 1024 * 1024
ACCESS_MODES = ('ReadWriteOnce', 'ReadWriteMany')
RECLAIM_ANNOTATION = 'ai-sdlc.io/original-reclaim-policy'
RUNTIME_CLASS = 'ai-sdlc-offline'
HOSTNAME = 'kubernetes.io/hostname'


def dependencies(manifest):
    """The verification and dependencies sections of a manifest."""
    verification = manifest.get('verification') if isinstance(manifest, dict) else None
    section = verification.get('dependencies') if isinstance(verification, dict) else None
    if not isinstance(section, dict) or set(section) != set(FIELDS):
        raise ValueError('The project manifest declares no complete verification.dependencies section')
    if not PINNED_IMAGE.fullmatch(str(verification.get('image', ''))):
        raise ValueError('verification.dependencies needs a verification image pinned by digest')
    globs = section['keyGlobs']
    if not isinstance(globs, list) or not 0 < len(globs) <= 32 or any(not isinstance(g, str) or not g for g in globs):
        raise ValueError('verification.dependencies.keyGlobs must list one to 32 globs')
    if not isinstance(section['cachePath'], str) or not section['cachePath'].startswith('/'):
        raise ValueError('verification.dependencies.cachePath must be absolute')
    return verification, section


def canonical(section):
    """The section's canonical JSON as Go encodes it (struct order, no HTML
    escaping, U+2028 and U+2029 escaped)."""
    text = json.dumps({name: section[name] for name in FIELDS}, separators=(',', ':'), ensure_ascii=False)
    return text.replace('\u2028', '\\u2028').replace('\u2029', '\\u2029').encode()


def match_glob(pattern, path):
    """project.matchGlob: '**' matches zero or more whole segments, '*' and '?'
    stay within one segment."""
    parts = path.split('/')
    reach = [True] + [False] * len(parts)
    for segment in pattern.split('/'):
        if segment == '**':
            seen, following = False, []
            for value in reach:
                seen = seen or value
                following.append(seen)
        else:
            following = [False] + [reach[j - 1] and fnmatch.fnmatchcase(parts[j - 1], segment)
                                   for j in range(1, len(parts) + 1)]
        reach = following
    return reach[-1]


def dependency_key(verification, section, files):
    """project.Verification.DependencyKey over {path: bytes}."""
    lines = [KEY_VERSION, 'image ' + verification['image'], 'settings ' + hashlib.sha256(canonical(section)).hexdigest()]
    for path in sorted(files):
        if any(match_glob(g, path) for g in section['keyGlobs']):
            lines.append(hashlib.sha256(files[path]).hexdigest() + '  ' + path)
    return hashlib.sha256(('\n'.join(lines) + '\n').encode()).hexdigest()


def key_files(run, repository, revision, globs):
    """The contents of the key files at revision as the runtime reads them.

    The runtime fetches the forge's archive of the revision, which git archive
    produces: .gitattributes export-ignore, export-subst and end-of-line
    conversion apply. Reading the same git archive output here (with LF line
    endings and no local autocrlf) makes both sides hash the same bytes."""
    if not REVISION.fullmatch(revision):
        raise ValueError('The base revision must be a full commit SHA')
    archive = run(['git', '-c', 'core.autocrlf=false', '-c', 'core.eol=lf', '-C', str(repository), 'archive',
                   '--format=tar', revision], capture_output=True, check=True).stdout
    if len(archive) > MAX_ARCHIVE_BYTES:
        raise ValueError('The base revision archive exceeds its bound')
    files = {}
    with tarfile.open(fileobj=io.BytesIO(archive), mode='r:') as tar:
        for member in tar:
            if member.isfile() and any(match_glob(g, member.name) for g in globs):
                files[member.name] = tar.extractfile(member).read()
    return files


def claim_name(key):
    if not KEY.fullmatch(key):
        raise ValueError('Invalid dependency key')
    return 'deps-' + key


def revision_key(run, manifest, repository, revision):
    verification, section = dependencies(manifest)
    return dependency_key(verification, section, key_files(run, repository, revision, section['keyGlobs']))


def work_claim(key, namespace, size, storage_class=None, access_mode='ReadWriteOnce'):
    """The empty claim the maven-dependencies Task fills. ReadWriteOnce
    suits node-local storage: on a single node, or on a cluster with workers
    where the volume's node affinity keeps every pod that mounts it (the fill
    task and the three concurrent verification runs) on its worker, and seal
    checks that worker can run sandboxed pods. A ReadWriteMany class lets the
    runs attach the sealed volume on any node instead."""
    if (not NAMESPACE.fullmatch(namespace) or namespace == VERIFIER_NAMESPACE or not SIZE.fullmatch(size)
            or access_mode not in ACCESS_MODES):
        raise ValueError('Invalid work namespace, size or access mode')
    claim = {'apiVersion': 'v1', 'kind': 'PersistentVolumeClaim',
             'metadata': {'name': claim_name(key), 'namespace': namespace,
                          'labels': {'app.kubernetes.io/part-of': 'ai-sdlc', STATE_LABEL: 'filling'},
                          'annotations': {KEY_ANNOTATION: key}},
             'spec': {'accessModes': [access_mode], 'resources': {'requests': {'storage': size}}}}
    if storage_class:
        claim['spec']['storageClassName'] = storage_class
    return claim


def sealed_claim(key, volume):
    """The verifier namespace claim that binds the handed-over volume."""
    spec = volume['spec']
    claim = {'apiVersion': 'v1', 'kind': 'PersistentVolumeClaim',
             'metadata': {'name': claim_name(key), 'namespace': VERIFIER_NAMESPACE,
                          'labels': {'app.kubernetes.io/part-of': 'ai-sdlc', STATE_LABEL: 'sealed'},
                          'annotations': {KEY_ANNOTATION: key}},
             'spec': {'accessModes': list(spec['accessModes']), 'volumeName': volume['metadata']['name'],
                      'storageClassName': spec.get('storageClassName', ''),
                      'resources': {'requests': {'storage': spec['capacity']['storage']}}}}
    if not claim['spec']['accessModes'] or any(mode not in ('ReadOnlyMany', 'ReadWriteMany', 'ReadWriteOnce')
                                               for mode in claim['spec']['accessModes']):
        raise ValueError('The cache volume must be ReadWriteOnce, ReadWriteMany or ReadOnlyMany')
    return claim


class Cluster:
    """kubectl for one context; every call is bounded and checked."""

    def __init__(self, run, context):
        if not context or context.startswith('-') or len(context) > 253:
            raise ValueError('Invalid kubectl context')
        self.run, self.context = run, context

    def kubectl(self, *args, input=None, check=True):
        return self.run(['kubectl', '--context', self.context, '--request-timeout=30s', *args],
                        input=input, capture_output=True, check=check)

    def get(self, kind, name, namespace=None):
        args = ['get', kind, name, '-o', 'json'] + (['-n', namespace] if namespace else [])
        result = self.kubectl(*args, check=False)
        return json.loads(result.stdout) if result.returncode == 0 else None

    def create(self, objects):
        self.kubectl('create', '-f', '-', input=json.dumps({'apiVersion': 'v1', 'kind': 'List', 'items': objects}).encode())

    def wait(self, predicate, what, seconds, sleep=time.sleep):
        for _ in range(seconds):
            value = predicate()
            if value:
                return value
            sleep(1)
        raise RuntimeError('Timed out waiting for ' + what)


def prepare(cluster, key, namespace=CI_NAMESPACE, size='8Gi', storage_class=None, access_mode='ReadWriteOnce'):
    """Create the work claim; refuse when a sealed cache already exists."""
    name = claim_name(key)
    claim = work_claim(key, namespace, size, storage_class, access_mode)
    if cluster.get('pvc', name, VERIFIER_NAMESPACE):
        raise ValueError('The cache %s is already sealed' % name)
    if not cluster.get('pvc', name, namespace):
        cluster.create([claim])
    return name


def handed_volume(cluster, key):
    """The volume a seal of key has already retained, wherever its claimRef
    points (the work claim, or the not yet created sealed claim)."""
    volumes = json.loads(cluster.kubectl('get', 'pv', '-o', 'json').stdout).get('items', [])
    found = [v for v in volumes if v['metadata'].get('annotations', {}).get(KEY_ANNOTATION) == key]
    if len(found) > 1:
        raise RuntimeError('More than one volume is marked for this key')
    return found[0] if found else None


def retain(cluster, volume_name, key, attempts=5):
    """Record the key and the original reclaim policy and retain the volume
    in one patch that applies only to the version just read. A concurrent
    seal that got there first is detected on reread, so the recorded policy
    is always the volume's original one and deleting the work claim can never
    delete the cache."""
    for _ in range(attempts):
        volume = cluster.get('pv', volume_name)
        if not volume:
            raise RuntimeError('The work claim volume is missing')
        annotations = dict(volume['metadata'].get('annotations') or {})
        if annotations.get(KEY_ANNOTATION) == key and annotations.get(RECLAIM_ANNOTATION):
            return volume
        if annotations.get(KEY_ANNOTATION) or annotations.get(RECLAIM_ANNOTATION):
            raise RuntimeError('Volume %s carries another seal' % volume_name)
        annotations.update({KEY_ANNOTATION: key, RECLAIM_ANNOTATION: volume['spec'].get('persistentVolumeReclaimPolicy', 'Delete')})
        patch = [{'op': 'test', 'path': '/metadata/resourceVersion', 'value': volume['metadata']['resourceVersion']},
                 {'op': 'add', 'path': '/metadata/annotations', 'value': annotations},
                 {'op': 'replace', 'path': '/spec/persistentVolumeReclaimPolicy', 'value': 'Retain'}]
        if cluster.kubectl('patch', 'pv', volume_name, '--type', 'json', '-p', json.dumps(patch), check=False).returncode == 0:
            return cluster.get('pv', volume_name) or volume
    raise RuntimeError('Volume %s kept changing; rerun seal' % volume_name)


def finish(cluster, key, volume_name, sleep):
    """Wait for the sealed claim to bind, check it, and restore the volume's
    original reclaim policy."""
    name = claim_name(key)
    claim = cluster.wait(lambda: (lambda c: c if c and c['status'].get('phase') == 'Bound' else None)(
        cluster.get('pvc', name, VERIFIER_NAMESPACE)), 'the sealed claim to bind', 120, sleep)
    if (claim['metadata'].get('annotations', {}).get(KEY_ANNOTATION) != key
            or claim['metadata'].get('labels', {}).get(STATE_LABEL) != 'sealed'
            or claim['spec'].get('volumeName') != volume_name):
        raise RuntimeError('The claim %s/%s is not the sealed cache of this key' % (VERIFIER_NAMESPACE, name))
    volume = cluster.get('pv', volume_name)
    if volume is None:
        # Without a confirmed read the original policy may still be pending;
        # fail so a rerun finishes the restoration.
        raise RuntimeError('Could not read volume %s; rerun seal to finish' % volume_name)
    annotations = volume.get('metadata', {}).get('annotations') or {}
    if RECLAIM_ANNOTATION in annotations or KEY_ANNOTATION in annotations:
        # Restore the original policy and drop the seal's markers, so a volume
        # that later outlives its claim (an original Retain) is never taken
        # for an interrupted seal.
        patch = {'metadata': {'annotations': {RECLAIM_ANNOTATION: None, KEY_ANNOTATION: None}}}
        if annotations.get(RECLAIM_ANNOTATION):
            patch['spec'] = {'persistentVolumeReclaimPolicy': annotations[RECLAIM_ANNOTATION]}
        cluster.kubectl('patch', 'pv', volume_name, '--type', 'merge', '-p', json.dumps(patch))
    return name


def volume_nodes(volume):
    """Hostnames a node-local volume is bound to (its required node
    affinity), or None for a volume reachable from any node."""
    terms = (((volume.get('spec') or {}).get('nodeAffinity') or {}).get('required') or {}).get('nodeSelectorTerms') or []
    if not terms:
        return None
    hosts = set()
    for term in terms:
        expressions = term.get('matchExpressions') or []
        if term.get('matchFields') or not expressions or any(
                e.get('key') != HOSTNAME or e.get('operator') != 'In' for e in expressions):
            raise RuntimeError('The cache volume has a node affinity this tool cannot check')
        # Expressions of one term are ANDed; terms are ORed.
        term_hosts = None
        for expression in expressions:
            values = set(expression.get('values') or [])
            term_hosts = values if term_hosts is None else term_hosts & values
        hosts.update(term_hosts or set())
    return sorted(hosts)


def require_sandbox_node(cluster, volume):
    """The verification runs that mount the sealed claim use the offline
    RuntimeClass. A node-local volume (local-path, ReadWriteOnce) draws every
    pod that mounts it to its node through the volume's node affinity, so all
    three concurrent runs (baseline, cross, candidate) land there. On a
    cluster with workers the RuntimeClass schedules only onto prepared nodes;
    a volume filled anywhere else would leave every verification of the key
    pending, so it is refused before the hand-over."""
    # A checked read: an API failure must not look like an unrestricted class.
    result = cluster.kubectl('get', 'runtimeclass', RUNTIME_CLASS, '--ignore-not-found', '-o', 'json', check=False)
    if result.returncode != 0:
        raise RuntimeError('Cannot read RuntimeClass %s; refusing to hand over the cache' % RUNTIME_CLASS)
    runtime = json.loads(result.stdout) if result.stdout.strip() else None
    selector = ((runtime or {}).get('scheduling') or {}).get('nodeSelector') or {}
    hosts = volume_nodes(volume)
    if not selector or hosts is None:
        return None
    for host in hosts:
        labels = ((cluster.get('node', host) or {}).get('metadata') or {}).get('labels') or {}
        if all(labels.get(k) == v for k, v in selector.items()):
            return host
    raise RuntimeError('The cache volume is bound to %s, where sandboxed verification pods cannot run; fill it '
                       'with a Tekton run on a sandbox node' % ', '.join(hosts))


def seal(cluster, key, namespace=CI_NAMESPACE, sleep=time.sleep):
    """Move the filled volume from the work claim to the sealed verifier claim.

    Each step is recorded on the volume (the key and the original reclaim
    policy as annotations) before the next one, so a rerun resumes from any
    interruption and finishes by checking the sealed claim and restoring the
    original policy."""
    name = claim_name(key)
    if not NAMESPACE.fullmatch(namespace) or namespace == VERIFIER_NAMESPACE:
        raise ValueError('Invalid work namespace')
    sealed = cluster.get('pvc', name, VERIFIER_NAMESPACE)
    if sealed:
        if not sealed['spec'].get('volumeName'):
            raise RuntimeError('The claim %s/%s is not bound to a volume' % (VERIFIER_NAMESPACE, name))
        return finish(cluster, key, sealed['spec']['volumeName'], sleep)
    volume = handed_volume(cluster, key)
    work = cluster.get('pvc', name, namespace)
    if work:
        if (work['status'].get('phase') != 'Bound' or work['metadata'].get('annotations', {}).get(KEY_ANNOTATION) != key
                or work['metadata'].get('labels', {}).get(STATE_LABEL) != 'filling'
                or (volume is not None and work['spec'].get('volumeName') != volume['metadata']['name'])):
            raise RuntimeError('No bound work claim %s/%s for this key' % (namespace, name))
        pods = json.loads(cluster.kubectl('get', 'pods', '-n', namespace, '-o', 'json').stdout).get('items', [])
        mounting = [p for p in pods if any(v.get('persistentVolumeClaim', {}).get('claimName') == name
                                           for v in p.get('spec', {}).get('volumes', []))]
        if any(p.get('status', {}).get('phase') not in ('Succeeded', 'Failed') for p in mounting):
            raise RuntimeError('A pod still uses %s; seal it after the dependency task has finished' % name)
        if mounting:
            # PVC protection keeps a claim that a finished pod still
            # references from being deleted, so the hand-over would hang.
            raise RuntimeError('Finished pods still reference %s (%s); delete them (for a Tekton run, delete its '
                               'PipelineRun) and rerun seal'
                               % (name, ', '.join(sorted(p.get('metadata', {}).get('name', '?') for p in mounting))))
    elif volume is None:
        raise RuntimeError('No bound work claim %s/%s for this key' % (namespace, name))
    require_sandbox_node(cluster, volume or cluster.get('pv', work['spec']['volumeName']) or {})
    if volume is None:
        volume = retain(cluster, work['spec']['volumeName'], key)
    volume_name = volume['metadata']['name']
    if cluster.get('pvc', name, namespace):
        cluster.kubectl('delete', 'pvc', name, '-n', namespace, '--wait=false')
        cluster.wait(lambda: cluster.get('pvc', name, namespace) is None, 'the work claim to be deleted', 300, sleep)
    volume = cluster.get('pv', volume_name)
    reference = volume['spec'].get('claimRef') or {}
    if (reference.get('namespace'), reference.get('name')) != (VERIFIER_NAMESPACE, name):
        cluster.wait(lambda: (lambda v: v if v and v['status'].get('phase') == 'Released' else None)(cluster.get('pv', volume_name)),
                     'the volume to be released', 120, sleep)
        cluster.kubectl('patch', 'pv', volume_name, '--type', 'json', '-p', json.dumps([
            {'op': 'replace', 'path': '/spec/claimRef', 'value': {'apiVersion': 'v1', 'kind': 'PersistentVolumeClaim',
                                                                    'namespace': VERIFIER_NAMESPACE, 'name': name}}]))
    cluster.create([sealed_claim(key, volume)])
    return finish(cluster, key, volume_name, sleep)


def main(argv=None, run=subprocess.run):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('action', choices=('key', 'prepare', 'seal'))
    parser.add_argument('--project-manifest', type=Path, required=True)
    parser.add_argument('--source', type=Path, required=True, help='a local clone that contains the base revision')
    parser.add_argument('--revision', required=True, help='the base revision (full commit SHA)')
    parser.add_argument('--context', help='kubectl context of the installation cluster (prepare, seal)')
    parser.add_argument('--namespace', default=CI_NAMESPACE, help='namespace of the work claim (default %(default)s)')
    parser.add_argument('--size', default='8Gi', help='work claim size (prepare; default %(default)s)')
    parser.add_argument('--storage-class', help='StorageClass of the work claim (prepare; default: the cluster default)')
    parser.add_argument('--access-mode', choices=ACCESS_MODES, default='ReadWriteOnce',
                        help='work claim access mode (prepare): ReadWriteOnce with node-local storage whose volumes '
                             'carry node affinity (for example local-path) keeps the fill and every verification run on '
                             'the volume\'s node, and seal checks that node can run sandboxed pods; multi-node block '
                             'storage without that affinity fails with Multi-Attach errors; ReadWriteMany lets them use '
                             'any node (default %(default)s)')
    args = parser.parse_args(argv)
    raw = args.project_manifest.read_bytes()
    if len(raw) > 16384:
        raise ValueError('Project manifest exceeds 16 KiB')
    key = revision_key(run, json.loads(raw), args.source, args.revision)
    if args.action == 'key':
        print(claim_name(key))
        return 0
    cluster = Cluster(run, args.context or '')
    if args.action == 'prepare':
        print(prepare(cluster, key, args.namespace, args.size, args.storage_class, args.access_mode))
    else:
        print(seal(cluster, key, args.namespace))
    return 0


if __name__ == '__main__':
    sys.exit(main())
