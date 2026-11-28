import copy
import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

import dependency_cache_install as deps

IMAGE = 'docker.io/library/maven@sha256:' + 'b' * 64
SECTION = {'cachePath': '/deps', 'keyGlobs': ['**/pom.xml', '.mvn/**', 'mvnw']}
MANIFEST = {'version': 'ai-sdlc.project/v1', 'repository': 'example/service', 'baseBranch': 'main',
            'verification': {'image': IMAGE, 'command': ['mvn', '-o', 'test'], 'dependencies': SECTION}}
# The vector of project.TestDependencyKeyIsStableAndCoversItsInputs.
FILES = {'rest-villains/pom.xml': b'<project>villains</project>\n', 'pom.xml': b'<project>root</project>\n',
         '.mvn/wrapper/maven-wrapper.properties': b'distributionType=only-script\n',
         'rest-villains/src/main/java/A.java': b'class A {}\n'}
GO_VECTOR = 'dc42c7e381a6295819ee1e442d42f621130901df35f6b4930b5598a5d28accea'


def repository(directory):
    git = lambda *a: subprocess.run(['git', '-C', directory, '-c', 'user.name=t', '-c', 'user.email=t@example.invalid', *a],
                                    check=True, capture_output=True).stdout.decode().strip()
    git('init', '-q')
    for path, data in FILES.items():
        (Path(directory) / path).parent.mkdir(parents=True, exist_ok=True)
        (Path(directory) / path).write_bytes(data)
    git('add', '-A')
    git('commit', '-q', '-m', 'base')
    return git('rev-parse', 'HEAD')


class DependencyKeyTests(unittest.TestCase):
    def test_key_matches_the_runtime(self):
        verification, section = deps.dependencies(MANIFEST)
        self.assertEqual(deps.dependency_key(verification, section, FILES), GO_VECTOR)
        changed = dict(FILES, **{'rest-villains/src/main/java/A.java': b'class A { int x; }\n'})
        self.assertEqual(deps.dependency_key(verification, section, changed), GO_VECTOR)
        for name, files in {'pom': dict(FILES, **{'pom.xml': b'<project>2</project>\n'}),
                            'new pom': dict(FILES, **{'rest-heroes/pom.xml': b''})}.items():
            self.assertNotEqual(deps.dependency_key(verification, section, files), GO_VECTOR, name)
        other = copy.deepcopy(MANIFEST)
        other['verification']['dependencies']['cachePath'] = '/m2'
        self.assertNotEqual(deps.dependency_key(*deps.dependencies(other), FILES), GO_VECTOR)

    def test_canonical_section_uses_go_struct_order(self):
        shuffled = dict(reversed(list(SECTION.items())))
        self.assertEqual(deps.canonical(shuffled), json.dumps(SECTION, separators=(',', ':')).encode())
        self.assertIn(b'\\u2028', deps.canonical(dict(SECTION, keyGlobs=['a '])))

    def test_globs_match_like_the_runtime(self):
        for pattern, path, want in [('**/pom.xml', 'pom.xml', True), ('**/pom.xml', 'a/b/pom.xml', True),
                                    ('**/pom.xml', 'a/pom.xml.bak', False), ('.mvn/**', '.mvn/wrapper/x', True),
                                    ('.mvn/**', '.mvn', True), ('mvnw', 'mvnw.cmd', False), ('*/pom.xml', 'a/b/pom.xml', False),
                                    ('a/**/b/**/pom.xml', 'a/b/pom.xml', True), ('src/*.xml', 'src/a/b.xml', False)]:
            self.assertEqual(deps.match_glob(pattern, path), want, (pattern, path))

    def test_key_files_read_the_object_database_not_the_work_tree(self):
        with tempfile.TemporaryDirectory() as directory:
            revision = repository(directory)
            (Path(directory) / 'pom.xml').write_bytes(b'uncommitted')
            files = deps.key_files(subprocess.run, directory, revision, SECTION['keyGlobs'])
            self.assertEqual(set(files), {'pom.xml', 'rest-villains/pom.xml', '.mvn/wrapper/maven-wrapper.properties'})
            self.assertEqual(deps.revision_key(subprocess.run, MANIFEST, directory, revision), GO_VECTOR)
            with self.assertRaises(ValueError):
                deps.key_files(subprocess.run, directory, 'HEAD', SECTION['keyGlobs'])

    def test_key_files_follow_the_archive_attributes(self):
        """The runtime reads the forge's git archive, so export-ignore applies."""
        with tempfile.TemporaryDirectory() as directory:
            repository(directory)
            git = lambda *a: subprocess.run(['git', '-C', directory, '-c', 'user.name=t', '-c', 'user.email=t@example.invalid', *a],
                                            check=True, capture_output=True).stdout.decode().strip()
            (Path(directory) / '.gitattributes').write_text('rest-villains/pom.xml export-ignore\n')
            git('add', '-A')
            git('commit', '-q', '-m', 'attributes')
            files = deps.key_files(subprocess.run, directory, git('rev-parse', 'HEAD'), SECTION['keyGlobs'])
            self.assertEqual(set(files), {'pom.xml', '.mvn/wrapper/maven-wrapper.properties'})

    def test_incomplete_sections_are_refused(self):
        for name, change in {
            'missing': lambda v: v.pop('dependencies'),
            'mutable image': lambda v: v.update(image='maven:latest'),
            'extra field': lambda v: v['dependencies'].update(prefetch=[['mvn']]),
            'globs': lambda v: v['dependencies'].update(keyGlobs=[]),
            'relative cache': lambda v: v['dependencies'].update(cachePath='deps'),
        }.items():
            manifest = copy.deepcopy(MANIFEST)
            change(manifest['verification'])
            with self.assertRaises(ValueError, msg=name):
                deps.dependencies(manifest)

    def test_claims(self):
        claim = deps.work_claim(GO_VECTOR, 'ai-sdlc-ci', '8Gi', 'standard')
        self.assertEqual(claim['metadata']['labels']['ai-sdlc.io/dependency-state'], 'filling')
        self.assertEqual(claim['metadata']['annotations'], {'ai-sdlc.io/dependency-key': GO_VECTOR})
        self.assertEqual(claim['spec']['storageClassName'], 'standard')
        for bad in (('ai-sdlc-verifiers', '8Gi'), ('ai-sdlc-ci', '8Ti'), ('Bad', '8Gi')):
            with self.assertRaises(ValueError):
                deps.work_claim(GO_VECTOR, *bad)
        volume = {'metadata': {'name': 'pv-1'}, 'spec': {'accessModes': ['ReadWriteOnce'], 'storageClassName': 'standard',
                                                          'capacity': {'storage': '8Gi'}}}
        sealed = deps.sealed_claim(GO_VECTOR, volume)
        self.assertEqual(sealed['metadata'], {'name': 'deps-' + GO_VECTOR, 'namespace': 'ai-sdlc-verifiers',
                                              'labels': {'app.kubernetes.io/part-of': 'ai-sdlc', 'ai-sdlc.io/dependency-state': 'sealed'},
                                              'annotations': {'ai-sdlc.io/dependency-key': GO_VECTOR}})
        self.assertEqual(sealed['spec']['volumeName'], 'pv-1')
        volume['spec']['accessModes'] = ['ReadWriteOncePod']
        with self.assertRaises(ValueError):
            deps.sealed_claim(GO_VECTOR, volume)
        with self.assertRaises(ValueError):
            deps.claim_name('../x')


class FakeCluster:
    """A small API server: claims, volumes and the binder, with an optional
    failure injected at the n-th mutating call (before or after it applies)."""

    def __init__(self, reclaim='Delete'):
        self.calls, self.pods, self.bind = [], [], True
        self.claims, self.volumes = {}, {'pv-1': {'metadata': {'name': 'pv-1', 'annotations': {}, 'resourceVersion': '1'}, 'status': {'phase': 'Available'},
                                                  'spec': {'accessModes': ['ReadWriteOnce'], 'storageClassName': 'standard',
                                                           'capacity': {'storage': '8Gi'}, 'persistentVolumeReclaimPolicy': reclaim}}}
        self.mutations, self.fail_at, self.fail_after = 0, None, False

    def reconcile(self):
        for (namespace, name), claim in self.claims.items():
            if claim['status']['phase'] == 'Bound' or not self.bind:
                continue
            volume = self.volumes['pv-1']
            reference = volume['spec'].get('claimRef')
            wanted = claim['spec'].get('volumeName')
            if wanted is None and reference is None and volume['status']['phase'] == 'Available':
                claim['spec']['volumeName'] = 'pv-1'
                volume['spec']['claimRef'] = {'namespace': namespace, 'name': name}
            elif wanted != 'pv-1' or reference != {'apiVersion': 'v1', 'kind': 'PersistentVolumeClaim', 'namespace': namespace, 'name': name}:
                continue
            claim['status']['phase'] = 'Bound'
            volume['status']['phase'] = 'Bound'

    def reply(self, value):
        return SimpleNamespace(returncode=0 if value is not None else 1, stdout=json.dumps(value if value is not None else {}).encode())

    def run(self, args, input=None, capture_output=True, check=True):
        if args[0] == 'git':
            return subprocess.run(args, input=input, capture_output=capture_output, check=check)
        command = args[4:]
        self.calls.append(command)
        self.reconcile()
        if command[0] == 'get':
            if command[1] == 'pods':
                return self.reply({'items': self.pods})
            if command[1:3] == ['pv', '-o']:
                return self.reply({'items': list(self.volumes.values())})
            if command[1] == 'pv':
                return self.reply(copy.deepcopy(self.volumes.get(command[2])))
            return self.reply(copy.deepcopy(self.claims.get((command[-1], command[2]))))
        self.mutations += 1
        fail = self.mutations == self.fail_at
        if fail and not self.fail_after:
            raise subprocess.CalledProcessError(1, args)
        if command[0] == 'create':
            for item in json.loads(input)['items']:
                key = (item['metadata']['namespace'], item['metadata']['name'])
                if key in self.claims:
                    raise subprocess.CalledProcessError(1, args)
                self.claims[key] = dict(copy.deepcopy(item), status={'phase': 'Pending'})
        elif command[:2] == ['delete', 'pvc']:
            claim = self.claims.pop((command[4], command[2]))
            volume = self.volumes['pv-1']
            if claim['spec'].get('volumeName') == 'pv-1':
                if volume['spec']['persistentVolumeReclaimPolicy'] == 'Delete':
                    del self.volumes['pv-1']
                else:
                    volume['status']['phase'] = 'Released'
        elif command[:2] == ['patch', 'pv']:
            volume, patch = self.volumes[command[2]], json.loads(command[-1])
            if isinstance(patch, list):
                for op in patch:
                    if op['op'] == 'test' and volume['metadata']['resourceVersion'] != op['value']:
                        return SimpleNamespace(returncode=1, stdout=b'')
                for op in patch:
                    if op['path'] == '/spec/claimRef':
                        volume['spec']['claimRef'] = op['value']
                        volume['status']['phase'] = 'Available'
                    elif op['path'] == '/metadata/annotations':
                        volume['metadata']['annotations'] = dict(op['value'])
                    elif op['path'] == '/spec/persistentVolumeReclaimPolicy':
                        volume['spec']['persistentVolumeReclaimPolicy'] = op['value']
            else:
                for key, value in patch.get('metadata', {}).get('annotations', {}).items():
                    if value is None:
                        volume['metadata']['annotations'].pop(key, None)
                    else:
                        volume['metadata']['annotations'][key] = value
                volume['spec'].update(patch.get('spec', {}))
            volume['metadata']['resourceVersion'] = str(int(volume['metadata']['resourceVersion']) + 1)
        self.reconcile()
        if fail:
            raise subprocess.CalledProcessError(1, args)
        return SimpleNamespace(returncode=0, stdout=b'{}')


class SealTests(unittest.TestCase):
    def cluster(self, reclaim='Delete'):
        self.fake = FakeCluster(reclaim)
        return deps.Cluster(self.fake.run, 'kind-test')

    def assert_sealed(self, reclaim):
        claim = self.fake.claims[('ai-sdlc-verifiers', 'deps-' + GO_VECTOR)]
        self.assertEqual(claim['status']['phase'], 'Bound')
        self.assertEqual(claim['metadata']['labels']['ai-sdlc.io/dependency-state'], 'sealed')
        self.assertEqual(claim['metadata']['annotations'], {'ai-sdlc.io/dependency-key': GO_VECTOR})
        volume = self.fake.volumes['pv-1']
        self.assertEqual(volume['spec']['persistentVolumeReclaimPolicy'], reclaim)
        self.assertNotIn(deps.RECLAIM_ANNOTATION, volume['metadata']['annotations'])
        self.assertNotIn(deps.KEY_ANNOTATION, volume['metadata']['annotations'])
        self.assertNotIn(('ai-sdlc-ci', 'deps-' + GO_VECTOR), self.fake.claims)

    def test_prepare_then_seal_hands_the_volume_over(self):
        cluster = self.cluster()
        self.assertEqual(deps.prepare(cluster, GO_VECTOR), 'deps-' + GO_VECTOR)
        self.assertEqual(self.fake.claims[('ai-sdlc-ci', 'deps-' + GO_VECTOR)]['spec']['accessModes'], ['ReadWriteOnce'])
        self.fake.calls.clear()
        self.assertEqual(deps.seal(cluster, GO_VECTOR, sleep=lambda _: None), 'deps-' + GO_VECTOR)
        verbs = [' '.join(c[:3]) for c in self.fake.calls if c[0] != 'get']
        self.assertEqual(verbs, ['patch pv pv-1', 'delete pvc deps-' + GO_VECTOR, 'patch pv pv-1', 'create -f -', 'patch pv pv-1'])
        self.assertIn('--wait=false', next(c for c in self.fake.calls if c[:2] == ['delete', 'pvc']))
        self.assert_sealed('Delete')
        # A sealed cache is final: seal is idempotent and prepare refuses it.
        self.fake.calls.clear()
        self.assertEqual(deps.seal(cluster, GO_VECTOR, sleep=lambda _: None), 'deps-' + GO_VECTOR)
        self.assertTrue(all(c[0] == 'get' for c in self.fake.calls))
        with self.assertRaises(ValueError):
            deps.prepare(cluster, GO_VECTOR)

    def test_an_original_retain_policy_is_kept(self):
        cluster = self.cluster('Retain')
        deps.prepare(cluster, GO_VECTOR)
        deps.seal(cluster, GO_VECTOR, sleep=lambda _: None)
        self.assert_sealed('Retain')

    def test_every_interruption_resumes(self):
        for reclaim in ('Delete', 'Retain'):
            for after in (False, True):
                for step in range(1, 6):
                    with self.subTest(reclaim=reclaim, after=after, step=step):
                        cluster = self.cluster(reclaim)
                        deps.prepare(cluster, GO_VECTOR)
                        self.fake.mutations, self.fake.fail_at, self.fake.fail_after = 0, step, after
                        with self.assertRaises(subprocess.CalledProcessError):
                            deps.seal(cluster, GO_VECTOR, sleep=lambda _: None)
                        self.fake.fail_at = None
                        self.assertEqual(deps.seal(cluster, GO_VECTOR, sleep=lambda _: None), 'deps-' + GO_VECTOR)
                        self.assert_sealed(reclaim)

    def test_a_bind_timeout_is_finished_by_a_rerun(self):
        cluster = self.cluster()
        deps.prepare(cluster, GO_VECTOR)
        self.fake.reconcile()
        self.fake.bind = False
        with self.assertRaisesRegex(RuntimeError, 'sealed claim to bind'):
            deps.seal(cluster, GO_VECTOR, sleep=lambda _: None)
        self.assertEqual(self.fake.volumes['pv-1']['spec']['persistentVolumeReclaimPolicy'], 'Retain')
        self.fake.bind = True
        deps.seal(cluster, GO_VECTOR, sleep=lambda _: None)
        self.assert_sealed('Delete')

    def test_a_concurrent_seal_cannot_record_retain_as_the_original(self):
        cluster = self.cluster()
        deps.prepare(cluster, GO_VECTOR)
        self.fake.reconcile()
        real = self.fake.run
        raced = []
        def racing(args, **kwargs):
            command = args[4:]
            if command[:2] == ['patch', 'pv'] and '"test"' in command[-1] and not raced:
                # Another seal retains the volume between this run's read and patch.
                raced.append(True)
                volume = self.fake.volumes['pv-1']
                volume['metadata']['annotations'] = {deps.KEY_ANNOTATION: GO_VECTOR, deps.RECLAIM_ANNOTATION: 'Delete'}
                volume['spec']['persistentVolumeReclaimPolicy'] = 'Retain'
                volume['metadata']['resourceVersion'] = str(int(volume['metadata']['resourceVersion']) + 1)
            return real(args, **kwargs)
        self.fake.run = racing
        cluster = deps.Cluster(racing, 'kind-test')
        deps.seal(cluster, GO_VECTOR, sleep=lambda _: None)
        self.assertTrue(raced)
        self.assert_sealed('Delete')

    def test_an_unreadable_volume_is_not_reported_as_sealed(self):
        cluster = self.cluster()
        deps.prepare(cluster, GO_VECTOR)
        deps.seal(cluster, GO_VECTOR, sleep=lambda _: None)
        self.fake.volumes['pv-1']['metadata']['annotations'][deps.RECLAIM_ANNOTATION] = 'Delete'
        hidden = self.fake.volumes.pop('pv-1')
        with self.assertRaisesRegex(RuntimeError, 'Could not read volume'):
            deps.seal(cluster, GO_VECTOR, sleep=lambda _: None)
        self.fake.volumes['pv-1'] = hidden

    def test_a_foreign_sealed_claim_is_refused(self):
        cluster = self.cluster()
        deps.prepare(cluster, GO_VECTOR)
        deps.seal(cluster, GO_VECTOR, sleep=lambda _: None)
        self.fake.claims[('ai-sdlc-verifiers', 'deps-' + GO_VECTOR)]['metadata']['labels']['ai-sdlc.io/dependency-state'] = 'filling'
        with self.assertRaisesRegex(RuntimeError, 'not the sealed cache'):
            deps.seal(cluster, GO_VECTOR, sleep=lambda _: None)

    def test_seal_refuses_a_claim_in_use_or_of_another_key(self):
        cluster = self.cluster()
        deps.prepare(cluster, GO_VECTOR, access_mode='ReadWriteMany')
        self.assertEqual(self.fake.claims[('ai-sdlc-ci', 'deps-' + GO_VECTOR)]['spec']['accessModes'], ['ReadWriteMany'])
        self.fake.pods = [{'status': {'phase': 'Running'}, 'spec': {'volumes': [{'persistentVolumeClaim': {'claimName': 'deps-' + GO_VECTOR}}]}}]
        with self.assertRaisesRegex(RuntimeError, 'still uses'):
            deps.seal(cluster, GO_VECTOR, sleep=lambda _: None)
        self.fake.pods[0]['status']['phase'] = 'Succeeded'
        self.fake.claims[('ai-sdlc-ci', 'deps-' + GO_VECTOR)]['metadata']['annotations'][deps.KEY_ANNOTATION] = 'f' * 64
        with self.assertRaisesRegex(RuntimeError, 'No bound work claim'):
            deps.seal(cluster, GO_VECTOR, sleep=lambda _: None)
        with self.assertRaises(ValueError):
            deps.seal(cluster, GO_VECTOR, namespace='ai-sdlc-verifiers')
        with self.assertRaises(ValueError):
            deps.prepare(cluster, GO_VECTOR, access_mode='ReadWriteOncePod')
        self.assertFalse(any(c[0] == 'patch' for c in self.fake.calls))
        with self.assertRaises(ValueError):
            deps.Cluster(self.fake.run, '--kubeconfig=/x')

    def test_cli_key_needs_no_cluster(self):
        with tempfile.TemporaryDirectory() as directory, tempfile.NamedTemporaryFile('w', suffix='.json') as manifest:
            revision = repository(directory)
            json.dump(MANIFEST, manifest)
            manifest.flush()
            printed = []
            import builtins
            original, builtins.print = builtins.print, printed.append
            try:
                self.assertEqual(deps.main(['key', '--project-manifest', manifest.name, '--source', directory, '--revision', revision]), 0)
            finally:
                builtins.print = original
            self.assertEqual(printed, ['deps-' + GO_VECTOR])


if __name__ == '__main__':
    unittest.main()
