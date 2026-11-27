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
        volume['spec']['accessModes'] = ['ReadWriteMany']
        with self.assertRaises(ValueError):
            deps.sealed_claim(GO_VECTOR, volume)
        with self.assertRaises(ValueError):
            deps.claim_name('../x')


class FakeCluster:
    """Records kubectl calls and answers gets from a small object store."""

    def __init__(self):
        self.calls, self.store, self.pods, self.interrupt = [], {}, [], False

    def run(self, args, input=None, capture_output=True, check=True):
        command = args[4:]
        self.calls.append(command)
        if command[:2] == ['get', 'pods']:
            return SimpleNamespace(returncode=0, stdout=json.dumps({'items': self.pods}).encode())
        if command[:3] == ['get', 'pv', '-o']:
            return SimpleNamespace(returncode=0, stdout=json.dumps({'items': [v for k, v in self.store.items() if k.startswith('pv/')]}).encode())
        if command[0] == 'get':
            key = '/'.join([command[1], command[2]] + (command[-1:] if '-n' in command else []))
            value = self.store.get(key)
            return SimpleNamespace(returncode=0 if value else 1, stdout=json.dumps(value or {}).encode())
        if command[:2] == ['delete', 'pvc']:
            self.store.pop('pvc/%s/%s' % (command[2], command[4]), None)
            self.store['pv/pv-1']['status']['phase'] = 'Released'
            self.store['pv/pv-1']['spec']['claimRef'] = {'namespace': command[4], 'name': command[2]}
            if self.interrupt:
                raise subprocess.CalledProcessError(1, args)
        if command[:2] == ['patch', 'pv']:
            patch = json.loads(command[-1])
            if isinstance(patch, dict):
                self.store['pv/pv-1']['spec'].update(patch['spec'])
        if command[0] == 'create':
            for item in json.loads(input)['items']:
                spec = dict(item['spec'], volumeName=item['spec'].get('volumeName', 'pv-1'))
                self.store['pvc/%s/%s' % (item['metadata']['name'], item['metadata']['namespace'])] = dict(item, spec=spec, status={'phase': 'Bound'})
        return SimpleNamespace(returncode=0, stdout=b'{}')


class SealTests(unittest.TestCase):
    def setUp(self):
        self.fake = FakeCluster()
        self.fake.store['pv/pv-1'] = {'metadata': {'name': 'pv-1'}, 'status': {'phase': 'Bound'},
                                      'spec': {'accessModes': ['ReadWriteOnce'], 'storageClassName': 'standard', 'capacity': {'storage': '8Gi'}}}
        self.cluster = deps.Cluster(self.fake.run, 'kind-test')

    def test_prepare_then_seal_hands_the_volume_over(self):
        self.assertEqual(deps.prepare(self.cluster, GO_VECTOR), 'deps-' + GO_VECTOR)
        self.assertIn('pvc/deps-%s/ai-sdlc-ci' % GO_VECTOR, self.fake.store)
        self.fake.calls.clear()
        self.assertEqual(deps.seal(self.cluster, GO_VECTOR, sleep=lambda _: None), 'deps-' + GO_VECTOR)
        verbs = [' '.join(c[:3]) for c in self.fake.calls if c[0] != 'get']
        self.assertEqual(verbs, ['patch pv pv-1', 'delete pvc deps-' + GO_VECTOR, 'patch pv pv-1', 'create -f -', 'patch pv pv-1'])
        self.assertIn('--wait=false', next(c for c in self.fake.calls if c[:2] == ['delete', 'pvc']))
        patches = [json.loads(c[-1]) for c in self.fake.calls if c[:2] == ['patch', 'pv']]
        self.assertEqual(patches[0], {'spec': {'persistentVolumeReclaimPolicy': 'Retain'}})
        self.assertEqual(patches[1][0]['value'], {'apiVersion': 'v1', 'kind': 'PersistentVolumeClaim',
                                                  'namespace': 'ai-sdlc-verifiers', 'name': 'deps-' + GO_VECTOR})
        self.assertEqual(patches[2], {'spec': {'persistentVolumeReclaimPolicy': 'Delete'}})
        sealed = self.fake.store['pvc/deps-%s/ai-sdlc-verifiers' % GO_VECTOR]
        self.assertEqual(sealed['metadata']['labels']['ai-sdlc.io/dependency-state'], 'sealed')
        # A sealed cache is final: seal is idempotent and prepare refuses it.
        self.fake.calls.clear()
        self.assertEqual(deps.seal(self.cluster, GO_VECTOR, sleep=lambda _: None), 'deps-' + GO_VECTOR)
        self.assertTrue(all(c[0] == 'get' for c in self.fake.calls))
        with self.assertRaises(ValueError):
            deps.prepare(self.cluster, GO_VECTOR)

    def test_an_interrupted_seal_resumes_from_the_released_volume(self):
        deps.prepare(self.cluster, GO_VECTOR)
        self.fake.interrupt = True
        with self.assertRaises(subprocess.CalledProcessError):
            deps.seal(self.cluster, GO_VECTOR, sleep=lambda _: None)
        self.fake.interrupt = False
        self.assertNotIn('pvc/deps-%s/ai-sdlc-ci' % GO_VECTOR, self.fake.store)
        self.assertEqual(deps.seal(self.cluster, GO_VECTOR, sleep=lambda _: None), 'deps-' + GO_VECTOR)
        self.assertIn('pvc/deps-%s/ai-sdlc-verifiers' % GO_VECTOR, self.fake.store)
        self.assertEqual(self.fake.store['pv/pv-1']['spec']['persistentVolumeReclaimPolicy'], 'Delete')

    def test_seal_refuses_a_claim_in_use_or_of_another_key(self):
        deps.prepare(self.cluster, GO_VECTOR)
        self.fake.pods = [{'status': {'phase': 'Running'}, 'spec': {'volumes': [{'persistentVolumeClaim': {'claimName': 'deps-' + GO_VECTOR}}]}}]
        with self.assertRaisesRegex(RuntimeError, 'still uses'):
            deps.seal(self.cluster, GO_VECTOR, sleep=lambda _: None)
        self.fake.pods[0]['status']['phase'] = 'Succeeded'
        self.fake.store['pvc/deps-%s/ai-sdlc-ci' % GO_VECTOR]['metadata']['annotations'][deps.KEY_ANNOTATION] = 'f' * 64
        with self.assertRaisesRegex(RuntimeError, 'No bound work claim'):
            deps.seal(self.cluster, GO_VECTOR, sleep=lambda _: None)
        with self.assertRaises(ValueError):
            deps.seal(self.cluster, GO_VECTOR, namespace='ai-sdlc-verifiers')
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
