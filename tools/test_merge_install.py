import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import github_merge_policy as native
import install
import install_config
import test_publication_install


class MergeInstallTests(unittest.TestCase):
    def fixture(self):
        return {'id': native.RULESET, 'name': 'Main Branch Protection', 'source': native.REPOSITORY,
                'source_type': 'Repository', 'target': 'branch', 'enforcement': 'active',
                'bypass_actors': [{'actor_id': 5, 'actor_type': 'RepositoryRole', 'bypass_mode': 'pull_request'}],
                'conditions': {'ref_name': {'include': ['~DEFAULT_BRANCH'], 'exclude': []}},
                'rules': [{'type': 'deletion'}, {'type': 'non_fast_forward'},
                          {'type': 'commit_message_pattern', 'parameters': {'pattern': '^fix: ', 'operator': 'regex'}},
                          {'type': 'pull_request', 'parameters': {'required_approving_review_count': 0, 'required_review_thread_resolution': False}},
                          {'type': 'required_status_checks', 'parameters': {'strict_required_status_checks_policy': False,
                           'required_status_checks': [{'context': c, 'integration_id': a} for c, a in native.REQUIRED]}}]}

    def test_native_plan_does_not_write_and_preserves_other_rules(self):
        current = self.fixture()
        desired = native.desired(current)
        self.assertEqual(desired['rules'][:3], current['rules'][:3])
        self.assertEqual(desired['bypass_actors'], [])
        self.assertFalse(current['rules'][-1]['parameters']['strict_required_status_checks_policy'])
        with patch.object(native, '_api', return_value=current) as api:
            with self.assertRaises(ValueError):
                native.verify()
            api.assert_called_once_with()

    def test_verified_native_floor_is_read_only(self):
        current = self.fixture()
        updated = dict(current, **native.desired(current))
        with patch.object(native, '_api', return_value=updated) as api:
            self.assertTrue(native.verify()['verified'])
            api.assert_called_once_with()

    def test_native_drift_cannot_weaken_checks_scope_or_human_reviews(self):
        for field, value in [('source', 'other/repo'), ('bypass_actors', None), ('enforcement', 'evaluate'),
                             ('conditions', {'ref_name': {'include': ['~ALL'], 'exclude': []}})]:
            with self.assertRaises(ValueError):
                native.desired(dict(self.fixture(), **{field: value}))
        for index, field, value in [(-2, 'required_approving_review_count', 1), (-1, 'required_status_checks', [])]:
            current = self.fixture()
            current['rules'][index]['parameters'][field] = value
            with self.assertRaises(ValueError):
                native.desired(current)
    def test_merge_requires_explicit_verified_publication_and_native_auth(self):
        for argv in (['local', '--merge'], ['local', '--merge', '--publication', '--independent-review',
                                           '--profile', 'verification', '--repository', 'os-santiago/homedir']):
            with patch.object(install, 'tools') as tools:
                with self.assertRaises(ValueError):
                    install.main(argv)
                tools.assert_not_called()
        config = json.loads((install.ROOT / 'examples/homedir.local-install.json').read_text())
        config.update(profile='verification', independentReview=True, publication=True, merge=True)
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'config.json'
            path.write_text(json.dumps(config))
            values, digest = install_config.load(path)
            self.assertTrue(values['merge'])
            for change in ({'merge': 'true'}, {'publication': False}, {'profile': 'candidate'}):
                path.write_text(json.dumps(dict(config, **change)))
                with self.assertRaises(ValueError):
                    install_config.load(path)

    def test_old_migrator_cannot_silently_omit_merge_authority(self):
        lock = {'image': {'repository': 'quay.io/os-santiago/ai-sdlc'}, 'migrator': {'digest': 'sha256:' + 'a'*64}}
        with patch.object(install, 'identity_material', return_value={'policy': {'grants': []}}), patch.object(install, 'owned_secret') as read:
            with self.assertRaisesRegex(ValueError, 'Pinned migrator'):
                install.configure_identities([], 'podman', lock, 'uid', 'os-santiago/homedir', 'ai-sdlc-production',
                                             candidate=True, verification=True, publication=True, merge=True)
            read.assert_not_called()

    def test_helm_activation_requires_publication_and_does_not_add_an_external_route(self):
        renderer = test_publication_install.PublicationInstallTests()
        enabled = renderer.render(True, merge={'enabled': True, 'configJSON': '{}'})
        self.assertEqual(enabled.returncode, 0, enabled.stderr)
        self.assertIn('AI_SDLC_MERGE', enabled.stdout)
        self.assertNotIn('/internal/merge-publication', enabled.stdout)
        self.assertNotEqual(renderer.render(False, merge={'enabled': True, 'configJSON': '{}'}).returncode, 0)


if __name__ == '__main__':
    unittest.main()
