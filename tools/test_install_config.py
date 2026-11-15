import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import install
import install_config


class InstallationConfigurationTests(unittest.TestCase):
    def setUp(self):
        self.fixture = json.loads((install.ROOT / 'examples/homedir.local-install.json').read_text())

    def test_reviewed_homedir_plan_has_scope_and_no_credential_access(self):
        output = io.StringIO()
        with patch.object(install, 'tools', return_value={'helm': 'helm'}), patch.object(install, 'run') as run, contextlib.redirect_stdout(output):
            self.assertEqual(install.main(['plan', '--config', str(install.ROOT / 'examples/homedir.local-install.json')]), 0)
        plan = json.loads(output.getvalue())
        self.assertEqual(plan['repository'], 'os-santiago/homedir')
        self.assertEqual(plan['cluster'], 'ai-sdlc-homedir')
        self.assertEqual(plan['environment'], 'ai-sdlc-production')
        self.assertEqual(plan['mode'], 'shadow')
        self.assertEqual(plan['githubAuth'], 'none')
        self.assertFalse(plan['deliveryEnabled'])
        self.assertTrue(plan['configurationDigest'].startswith('sha256:'))
        self.assertEqual(run.call_count, 1)
        self.assertEqual(run.call_args.args[0][:2], ['helm', 'template'])

    def test_rejects_untrusted_configuration_before_tools(self):
        invalid = [dict(self.fixture, token='synthetic'), dict(self.fixture, profile='active'),
                   dict(self.fixture, repository='../repo'), dict(self.fixture, cluster='../host'),
                   dict(self.fixture, provider='shell'), dict(self.fixture, environment='../../prod'),
                   dict(self.fixture, apiVersion='v2'), dict(self.fixture, kind='RemoteInstallation'),
                   dict(self.fixture, images=None), dict(self.fixture, profile=True), []]
        raw_invalid = [json.dumps(item) for item in invalid] + [
            '{"profile":"maintenance","profile":"shadow"}', '{bad', ' ' * 16385]
        with tempfile.TemporaryDirectory() as tmp, patch.object(install, 'tools') as tools:
            path = Path(tmp) / 'install.json'
            for raw in raw_invalid:
                path.write_text(raw)
                with self.subTest(raw=raw[:80]), self.assertRaises(ValueError):
                    install.main(['local', '--config', str(path)])
            tools.assert_not_called()

    def test_conflicts_fail_before_cluster_or_auth_access(self):
        with patch.object(install, 'tools') as tools:
            for option, value in [('repository', 'different/repo'), ('cluster', 'different'),
                                  ('environment', 'real-production'), ('profile', 'maintenance'),
                                  ('images', '/tmp/another-lock.json'), ('provider', 'docker')]:
                with self.subTest(option=option), self.assertRaisesRegex(ValueError, 'conflicts'):
                    install.main(['local', '--config', str(install.ROOT / 'examples/homedir.local-install.json'),
                                  '--' + option, value, '--github-auth', 'gh-cli'])
            tools.assert_not_called()

    def test_relative_lock_resolution_and_canonical_digest(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'install.json'
            path.write_text(json.dumps(self.fixture))
            values, digest = install_config.load(path)
            self.assertEqual(values['images'], (path.parent / self.fixture['images']).resolve())
            path.write_text(json.dumps(self.fixture, sort_keys=True, indent=4))
            self.assertEqual(install_config.load(path)[1], digest)
            path.write_text(json.dumps(dict(self.fixture, repository='other/repository')))
            self.assertNotEqual(install_config.load(path)[1], digest)

    def test_same_config_addresses_status_without_switching_profile(self):
        import argparse
        args = argparse.Namespace(config=install.ROOT / 'examples/homedir.local-install.json',
                                  command='status', **{key: None for key in
                                      ('cluster', 'provider', 'images', 'repository', 'environment', 'profile')})
        install_config.resolve(args)
        self.assertEqual(args.cluster, 'ai-sdlc-homedir')
        self.assertIsNone(args.profile)


if __name__ == '__main__':
    unittest.main()
