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
    print the claim name for a base revision, computed from a local Git object
    database (``git ls-tree`` and ``git cat-file``, never a working tree);
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
import json
from pathlib import Path
import re
import subprocess
import sys
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
    """The contents of the key files at revision, read from the object database."""
    if not REVISION.fullmatch(revision):
        raise ValueError('The base revision must be a full commit SHA')
    listing = run(['git', '-C', str(repository), 'ls-tree', '-r', '-z', '--full-tree', revision],
                  capture_output=True, check=True).stdout
    files = {}
    for entry in listing.split(b'\0'):
        if not entry:
            continue
        meta, path = entry.split(b'\t', 1)
        kind, obj = meta.split()[1:3]
        name = path.decode()
        if kind != b'blob' or not any(match_glob(g, name) for g in globs):
            continue
        files[name] = run(['git', '-C', str(repository), 'cat-file', 'blob', obj.decode()],
                          capture_output=True, check=True).stdout
    return files


def claim_name(key):
    if not KEY.fullmatch(key):
        raise ValueError('Invalid dependency key')
    return 'deps-' + key


def revision_key(run, manifest, repository, revision):
    verification, section = dependencies(manifest)
    return dependency_key(verification, section, key_files(run, repository, revision, section['keyGlobs']))


def work_claim(key, namespace, size, storage_class=None):
    """The empty claim the maven-dependencies Task fills."""
    if not NAMESPACE.fullmatch(namespace) or namespace == VERIFIER_NAMESPACE or not SIZE.fullmatch(size):
        raise ValueError('Invalid work namespace or size')
    claim = {'apiVersion': 'v1', 'kind': 'PersistentVolumeClaim',
             'metadata': {'name': claim_name(key), 'namespace': namespace,
                          'labels': {'app.kubernetes.io/part-of': 'ai-sdlc', STATE_LABEL: 'filling'},
                          'annotations': {KEY_ANNOTATION: key}},
             'spec': {'accessModes': ['ReadWriteOnce'], 'resources': {'requests': {'storage': size}}}}
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
    if any(mode not in ('ReadOnlyMany', 'ReadWriteOnce') for mode in claim['spec']['accessModes']):
        raise ValueError('The cache volume must be ReadWriteOnce or ReadOnlyMany')
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


def prepare(cluster, key, namespace=CI_NAMESPACE, size='8Gi', storage_class=None):
    """Create the work claim; refuse when a sealed cache already exists."""
    name = claim_name(key)
    claim = work_claim(key, namespace, size, storage_class)
    if cluster.get('pvc', name, VERIFIER_NAMESPACE):
        raise ValueError('The cache %s is already sealed' % name)
    if not cluster.get('pvc', name, namespace):
        cluster.create([claim])
    return name


def released_volume(cluster, name, namespace):
    """A retained volume released by the work claim, from an interrupted seal."""
    volumes = json.loads(cluster.kubectl('get', 'pv', '-o', 'json').stdout).get('items', [])
    found = [v for v in volumes if (v['spec'].get('claimRef') or {}).get('namespace') == namespace
             and v['spec']['claimRef'].get('name') == name and v['spec'].get('persistentVolumeReclaimPolicy') == 'Retain']
    return found[0] if len(found) == 1 else None


def seal(cluster, key, namespace=CI_NAMESPACE, sleep=time.sleep):
    """Move the filled volume from the work claim to the sealed verifier claim.
    Every step is checked, so an interrupted seal resumes where it stopped."""
    name = claim_name(key)
    if not NAMESPACE.fullmatch(namespace) or namespace == VERIFIER_NAMESPACE:
        raise ValueError('Invalid work namespace')
    if cluster.get('pvc', name, VERIFIER_NAMESPACE):
        return name
    work = cluster.get('pvc', name, namespace)
    if work:
        if (work['status'].get('phase') != 'Bound' or work['metadata'].get('annotations', {}).get(KEY_ANNOTATION) != key
                or work['metadata'].get('labels', {}).get(STATE_LABEL) != 'filling'):
            raise RuntimeError('No bound work claim %s/%s for this key' % (namespace, name))
        pods = json.loads(cluster.kubectl('get', 'pods', '-n', namespace, '-o', 'json').stdout).get('items', [])
        if any(v.get('persistentVolumeClaim', {}).get('claimName') == name
               for p in pods if p.get('status', {}).get('phase') not in ('Succeeded', 'Failed')
               for v in p.get('spec', {}).get('volumes', [])):
            raise RuntimeError('A pod still uses %s; seal it after the dependency task has finished' % name)
        volume_name = work['spec']['volumeName']
        cluster.kubectl('patch', 'pv', volume_name, '--type', 'merge', '-p', json.dumps({'spec': {'persistentVolumeReclaimPolicy': 'Retain'}}))
        cluster.kubectl('delete', 'pvc', name, '-n', namespace, '--wait=false')
        cluster.wait(lambda: cluster.get('pvc', name, namespace) is None, 'the work claim to be deleted', 300, sleep)
    else:
        volume = released_volume(cluster, name, namespace)
        if not volume:
            raise RuntimeError('No bound work claim %s/%s for this key' % (namespace, name))
        volume_name = volume['metadata']['name']
    volume = cluster.wait(lambda: (lambda v: v if v and v['status'].get('phase') == 'Released' else None)(cluster.get('pv', volume_name)),
                          'the volume to be released', 120, sleep)
    claim = sealed_claim(key, volume)
    cluster.kubectl('patch', 'pv', volume_name, '--type', 'json', '-p', json.dumps([
        {'op': 'replace', 'path': '/spec/claimRef', 'value': {'apiVersion': 'v1', 'kind': 'PersistentVolumeClaim',
                                                                'namespace': VERIFIER_NAMESPACE, 'name': name}}]))
    cluster.create([claim])
    cluster.wait(lambda: (lambda c: c and c['status'].get('phase') == 'Bound')(cluster.get('pvc', name, VERIFIER_NAMESPACE)),
                 'the sealed claim to bind', 120, sleep)
    # Deleting the sealed claim later deletes the volume.
    cluster.kubectl('patch', 'pv', volume_name, '--type', 'merge', '-p', json.dumps({'spec': {'persistentVolumeReclaimPolicy': 'Delete'}}))
    return name


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
        print(prepare(cluster, key, args.namespace, args.size, args.storage_class))
    else:
        print(seal(cluster, key, args.namespace))
    return 0


if __name__ == '__main__':
    sys.exit(main())
