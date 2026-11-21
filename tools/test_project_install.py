import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

import github_merge_install
import github_merge_policy
import project_install

MANIFEST = {
    'version': 'ai-sdlc.project/v1', 'repository': 'example/service', 'baseBranch': 'main',
    'verification': {'image': 'registry.example/go@sha256:' + 'a' * 64, 'command': ['go', 'test', './...'],
                     'timeoutSeconds': 60, 'cpu': '250m', 'memory': '256Mi', 'ephemeralStorage': '512Mi',
                     'network': 'none', 'result': {'exitCode': True}, 'repeat': 2, 'testGlobs': ['**/*_test.go']},
    'publication': {'allow': ['**/*.go'], 'maxChangedFiles': 5, 'maxChangedBytes': 65536, 'maxFileBytes': 32768},
    'merge': {'rulesetID': 42, 'required': [{'context': 'test', 'appID': 15368}], 'strictChecks': True,
              'resolveThreads': True, 'humanApprovals': 0}}


def write(value):
    file = tempfile.NamedTemporaryFile('w', suffix='.json', delete=False)
    json.dump(value, file) if not isinstance(value, str) else file.write(value)
    file.close()
    return Path(file.name)


class ProjectManifestInstallTests(unittest.TestCase):
    def tearDown(self):
        github_merge_policy._PROJECT.clear()

    def test_load_accepts_only_the_installation_repository(self):
        path = write(MANIFEST)
        self.assertEqual(project_install.load(path, 'example/service')['repository'], 'example/service')
        with self.assertRaises(ValueError):
            project_install.load(path, 'example/other')
        reference = dict(MANIFEST, repository='os-santiago/homedir')
        with self.assertRaises(ValueError):
            project_install.load(write(reference), 'os-santiago/homedir')
        with self.assertRaises(ValueError):
            project_install.load(write('{"version":"ai-sdlc.project/v1","version":"x"}'), 'example/service')
        with self.assertRaises(ValueError):
            project_install.load(write(' ' * (project_install.MAX_BYTES + 1)), 'example/service')
        without = {k: v for k, v in MANIFEST.items() if k != 'verification'}
        with self.assertRaises(ValueError):
            project_install.load(write(without), 'example/service')

    def test_unsafe_scope_is_refused_before_any_remote_effect(self):
        cases = {
            'case-folded reference': dict(MANIFEST, repository='OS-Santiago/Saferus'),
            'uppercase': dict(MANIFEST, repository='Example/Service'),
            'other branch': dict(MANIFEST, baseBranch='develop'),
            'missing app': dict(MANIFEST, merge=dict(MANIFEST['merge'], required=[{'context': 'test'}])),
            'null app': dict(MANIFEST, merge=dict(MANIFEST['merge'], required=[{'context': 'test', 'appID': None}])),
            'approvals': dict(MANIFEST, merge=dict(MANIFEST['merge'], humanApprovals=1)),
            'no ruleset': dict(MANIFEST, merge=dict(MANIFEST['merge'], rulesetID='1')),
        }
        for name, manifest in cases.items():
            with self.assertRaises(ValueError, msg=name):
                project_install.load(write(manifest), manifest['repository'])

    def test_bind_keeps_an_existing_installed_manifest(self):
        compiled = {'manifestJSON': '{}', 'manifestSHA256': 'b' * 64, 'verifierQuota': {'requests.cpu': '810m'}}
        policy = {}
        values = project_install.bind(policy, compiled)
        self.assertEqual(policy['projectSHA256'], 'b' * 64)
        self.assertEqual(values, {'manifestJSON': '{}', 'verifierQuota': {'requests.cpu': '810m'}})
        with self.assertRaises(ValueError):
            project_install.bind({'projectSHA256': 'c' * 64}, compiled)

    def test_compile_uses_the_pinned_migrator_without_network(self):
        calls = []
        def run(args, **kwargs):
            calls.append((args, json.loads(kwargs['input'])))
            return SimpleNamespace(stdout=json.dumps({'version': 'ai-sdlc.project-installation/v1', 'manifestJSON': '{}', 'manifestSHA256': digest}))
        import hashlib
        digest = hashlib.sha256(b'{}').hexdigest()
        compiled = project_install.compile_installation(run, 'podman', 'registry.example/migrator@sha256:' + 'e' * 64, {'repository': 'example/service'}, MANIFEST)
        self.assertEqual(compiled['manifestSHA256'], digest)
        digest = 'd' * 64
        with self.assertRaises(ValueError):
            project_install.compile_installation(run, 'podman', 'registry.example/migrator@sha256:' + 'e' * 64, {'repository': 'example/service'}, MANIFEST)
        args, payload = calls[0]
        self.assertIn('--network=none', args)
        self.assertEqual(args[-1], 'compile-project-installation')
        self.assertEqual(payload['manifest']['repository'], 'example/service')

    def test_merge_floor_and_ruleset_come_from_the_manifest(self):
        github_merge_policy.register_project(MANIFEST)
        self.assertTrue(github_merge_policy.registered('example/service'))
        floor = github_merge_policy.manifest_for('example/service')
        self.assertEqual(floor['required'], [{'Context': 'test', 'AppID': 15368}])
        manifest, body, base, _ = github_merge_install.configuration('example/service')
        self.assertEqual(base, 'repos/example/service/rulesets')
        self.assertEqual(body['conditions']['ref_name']['include'], ['refs/heads/main'])
        policy = github_merge_install.validate(dict(body, id=42, source='example/service', source_type='Repository'), 'example/service')
        self.assertEqual(policy['RulesetID'], 42)
        project_install.require_ruleset(MANIFEST, policy)
        with self.assertRaisesRegex(ValueError, 'merge.rulesetID to 43'):
            project_install.require_ruleset(MANIFEST, dict(policy, RulesetID=43))
        with self.assertRaises(ValueError):
            github_merge_policy.manifest_for('example/other')

    def test_a_weakened_merge_floor_is_refused(self):
        weak = json.loads(json.dumps(MANIFEST))
        weak['merge']['humanApprovals'] = 1
        github_merge_policy.register_project(weak)
        with self.assertRaises(ValueError):
            github_merge_policy.manifest_for('example/service')


if __name__ == '__main__':
    unittest.main()
