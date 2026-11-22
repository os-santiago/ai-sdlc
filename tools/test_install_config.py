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

    def test_a_project_manifest_plans_publication_for_its_repository(self):
        manifest = {'version': 'ai-sdlc.project/v1', 'repository': 'example/service', 'baseBranch': 'main',
                    'verification': {}, 'merge': {'rulesetID': 1, 'required': [{'context': 'test', 'appID': 15368}],
                                                  'strictChecks': True, 'resolveThreads': True, 'humanApprovals': 0}}
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'project.json'
            path.write_text(json.dumps(manifest))
            config = dict(self.fixture, repository='example/service', profile='verification', independentReview=True,
                          publication=True, projectManifest='project.json', images=str((install.ROOT / 'examples' / self.fixture['images']).resolve()))
            (Path(directory) / 'install.json').write_text(json.dumps(config))
            output = io.StringIO()
            with patch.object(install, 'tools', return_value={'helm': 'helm'}), patch.object(install, 'run'), contextlib.redirect_stdout(output):
                self.assertEqual(install.main(['plan', '--config', str(Path(directory) / 'install.json')]), 0)
            plan = json.loads(output.getvalue())
            self.assertTrue(plan['publicationEnabled'])
            with patch.object(install, 'tools', return_value={'helm': 'helm', 'kind': 'kind', 'kubectl': 'kubectl'}), \
                 patch.object(install, 'run', side_effect=ValueError('stop at cluster access')):
                with self.assertRaisesRegex(ValueError, 'stop at cluster access'):
                    install.main(['status', '--config', str(Path(directory) / 'install.json')])
            self.assertEqual(plan['repository'], 'example/service')
            del config['projectManifest']
            (Path(directory) / 'install.json').write_text(json.dumps(config))
            with self.assertRaises(ValueError):
                install_config.load(Path(directory) / 'install.json')

    def test_rejects_untrusted_configuration_before_tools(self):
        invalid = [dict(self.fixture, token='synthetic'), dict(self.fixture, profile='active'),
                   dict(self.fixture, repository='../repo'), dict(self.fixture, cluster='../host'),
                   dict(self.fixture, provider='shell'), dict(self.fixture, environment='../../prod'),
                   dict(self.fixture, apiVersion='v2'), dict(self.fixture, kind='RemoteInstallation'),
                   dict(self.fixture, images=None), dict(self.fixture, profile=True),
                   dict(self.fixture, database='external'), dict(self.fixture, networking='open'),
                   dict(self.fixture, policyDigest='sha256:' + 'a' * 64),
                   dict(self.fixture, namespace='other-namespace'),
                   dict(self.fixture, timeout='0m'), dict(self.fixture, timeout='10h'),
                   dict(self.fixture, timeout='100000s'), dict(self.fixture, timeout=600),
                   dict(self.fixture, context='admin'),
                   dict(self.fixture, platform='openshift'),
                   dict(self.fixture, platform='openshift', kubeconfig='k.kubeconfig'),
                   dict(self.fixture, kubeconfig='k.kubeconfig', context='admin'),
                   dict(self.fixture, modelUpstream='upstream.json'),
                   dict(self.fixture, profile='shadow', modelUpstream='upstream.json'), []]
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

    def test_new_scope_conflicts_fail_before_tools(self):
        example = install.ROOT / 'examples/openshift.local-install.json'
        with patch.object(install, 'tools') as tools:
            for option, value in [('timeout', '5m'), ('context', 'other/context'),
                                  ('kubeconfig', '/tmp/other.kubeconfig'),
                                  ('platform', 'kind')]:
                argv = ['plan', '--config', str(example), '--' + option, value]
                with self.subTest(option=option), self.assertRaisesRegex(ValueError, 'conflicts|Invalid|does not'):
                    install.main(argv)
            tools.assert_not_called()

    def test_openshift_configuration_plans_an_existing_cluster_target(self):
        example = install.ROOT / 'examples/openshift.local-install.json'
        output = io.StringIO()
        with patch.object(install, 'tools', return_value={'helm': 'helm'}), patch.object(install, 'run') as run, contextlib.redirect_stdout(output):
            self.assertEqual(install.main(['plan', '--config', str(example)]), 0)
        plan = json.loads(output.getvalue())
        self.assertEqual(plan['platform'], 'openshift')
        self.assertEqual(plan['namespace'], 'ai-sdlc-runtime')
        self.assertEqual(plan['cluster'], 'ai-sdlc-openshift')
        self.assertEqual(plan['mode'], 'maintenance')
        self.assertEqual(plan['timeout'], '10m')
        self.assertFalse(plan['deliveryEnabled'])
        self.assertTrue(plan['configurationDigest'].startswith('sha256:'))

    def test_schema_and_loader_agree_on_the_full_documented_surface(self):
        import jsonschema
        schema = json.loads((install.ROOT / 'spec/local-install.schema.json').read_text())
        for example in sorted((install.ROOT / 'examples').glob('*.local-install.json')):
            fixture = json.loads(example.read_text())
            jsonschema.validate(fixture, schema)
            install_config.load(example)
        fixture = dict(self.fixture, platform='openshift', kubeconfig='k.kubeconfig',
                       context='admin/api.example.com:6443', timeout='5m',
                       namespace='ai-sdlc-runtime', modelUpstream='upstream.json',
                       profile='candidate')
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'install.json'
            path.write_text(json.dumps(fixture))
            jsonschema.validate(fixture, schema)
            values, _ = install_config.load(path)
            self.assertEqual(values['kubeconfig'], (path.parent / 'k.kubeconfig').resolve())
            self.assertEqual(values['model_upstream'], (path.parent / 'upstream.json').resolve())
            self.assertEqual(values['timeout'], '5m')
            self.assertEqual(values['namespace'], 'ai-sdlc-runtime')
            for changes in ({'platform': 'kind'}, {'kubeconfig': ''}, {'context': 'bad context'},
                            {'timeout': '90'}, {'timeout': '1h'}, {'namespace': 'other'},
                            {'modelUpstream': None}, {'profile': 'shadow'},
                            {'projectManifest': 'p.json'}, {'token': 'x'},
                            {'database': {'url': 'x'}}, {'unknown': 'y'}):
                invalid = dict(fixture, **changes)
                path.write_text(json.dumps(invalid))
                with self.subTest(changes=changes), self.assertRaises(jsonschema.ValidationError):
                    jsonschema.validate(invalid, schema)
                with self.assertRaises(ValueError):
                    install_config.load(path)

    def test_project_manifest_schema_and_loader_agree(self):
        import jsonschema
        schema = json.loads((install.ROOT / 'spec/local-install.schema.json').read_text())
        fixture = dict(self.fixture, repository='example/service', profile='verification',
                       independentReview=True, publication=True, projectManifest='project.json')
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'install.json'
            path.write_text(json.dumps(fixture))
            jsonschema.validate(fixture, schema)
            self.assertEqual(install_config.load(path)[0]['project_manifest'], (path.parent / 'project.json').resolve())
            for changes in ({'profile': 'candidate'}, {'repository': 'os-santiago/homedir'},
                            {'projectManifest': ''}, {'publication': 'yes'}):
                invalid = dict(fixture, **changes)
                path.write_text(json.dumps(invalid))
                with self.subTest(changes=changes), self.assertRaises(jsonschema.ValidationError):
                    jsonschema.validate(invalid, schema)
                with self.assertRaises(ValueError):
                    install_config.load(path)

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
