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

    def test_generation_budget_is_recorded_bounded_and_candidate_only(self):
        import jsonschema
        schema = json.loads((install.ROOT / 'spec/local-install.schema.json').read_text())
        candidate = json.loads((install.ROOT / 'examples/homedir-candidate.local-install.json').read_text())
        candidate['images'] = str((install.ROOT / 'examples' / candidate['images']).resolve())
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'install.json'
            jsonschema.validate(dict(candidate, generationSeconds=900, generationSteps=32), schema)
            selected = dict(candidate, generationSeconds=900, generationSteps=24)
            jsonschema.validate(selected, schema)
            path.write_text(json.dumps(selected))
            values, digest = install_config.load(path)
            self.assertEqual((values['generation_seconds'], values['generation_steps']), (900, 24))
            path.write_text(json.dumps(candidate))
            self.assertNotEqual(install_config.load(path)[1], digest)
            self.assertNotIn('generation_seconds', install_config.load(path)[0])
            # The plan renders the recorded budget into the candidate configuration.
            path.write_text(json.dumps(selected))
            rendered = []
            def run(command, **kwargs):
                rendered.append(json.loads(Path(command[-1]).read_text()))
            with patch.object(install, 'tools', return_value={'helm': 'helm'}), patch.object(install, 'run', side_effect=run), \
                 contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(install.main(['plan', '--config', str(path)]), 0)
            profile = json.loads(rendered[0]['candidate']['configJSON'])['profile']
            self.assertEqual((profile['maxSeconds'], profile['maxSteps']), (900, 24))
            # More than 24 steps needs a model upstream; the Codex path refuses before tools.
            path.write_text(json.dumps(dict(candidate, generationSteps=32)))
            with patch.object(install, 'tools') as tools:
                with self.assertRaisesRegex(ValueError, 'requires --model-upstream'):
                    install.main(['plan', '--config', str(path)])
                tools.assert_not_called()
            path.write_text(json.dumps(selected))
            with patch.object(install, 'tools') as tools:
                with self.assertRaisesRegex(ValueError, 'conflicts'):
                    install.main(['local', '--config', str(path), '--generation-seconds', '600'])
                tools.assert_not_called()
            # Status addresses the same scope without the plan/local-only option.
            with patch.object(install, 'tools', return_value={'helm': 'helm', 'kind': 'kind', 'kubectl': 'kubectl'}), \
                 patch.object(install, 'run', side_effect=ValueError('stop at cluster access')):
                with self.assertRaisesRegex(ValueError, 'stop at cluster access'):
                    install.main(['status', '--config', str(path)])
            invalid = [dict(candidate, generationSeconds=599), dict(candidate, generationSeconds=901),
                       dict(candidate, generationSeconds=True), dict(candidate, generationSeconds='900'),
                       dict(candidate, generationSteps=23), dict(candidate, generationSteps=33),
                       dict(candidate, generationSteps=24.0), dict(self.fixture, generationSeconds=900),
                       dict(self.fixture, generationSteps=32)]
            for config in invalid:
                path.write_text(json.dumps(config))
                with self.subTest(config=config), self.assertRaises(ValueError):
                    install_config.load(path)
                # JSON Schema accepts 24.0 as an integer; the parser rejects it.
                if not isinstance(config.get('generationSteps'), float):
                    with self.subTest(schema=config), self.assertRaises(jsonschema.ValidationError):
                        jsonschema.validate(config, schema)

    def test_answer_token_bound_is_recorded_bounded_and_candidate_only(self):
        import jsonschema
        schema = json.loads((install.ROOT / 'spec/local-install.schema.json').read_text())
        candidate = json.loads((install.ROOT / 'examples/homedir-candidate.local-install.json').read_text())
        candidate['images'] = str((install.ROOT / 'examples' / candidate['images']).resolve())
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'install.json'
            upstream = Path(tmp) / 'upstream.json'
            upstream.write_text(json.dumps({'adapter': 'openai', 'url': 'http://10.89.0.1:18090/v1/chat/completions',
                                            'model': 'local-model', 'cidr': '10.89.0.1/32', 'port': 18090,
                                            'seconds': 300, 'directAnswers': True, 'callSeconds': 330,
                                            'reviewCallSeconds': 170, 'credentialSecret': ''}))
            selected = dict(candidate, generationAnswerTokens=8192)
            jsonschema.validate(selected, schema)
            path.write_text(json.dumps(selected))
            values, digest = install_config.load(path)
            self.assertEqual(values['generation_answer_tokens'], 8192)
            path.write_text(json.dumps(candidate))
            self.assertNotEqual(install_config.load(path)[1], digest)
            self.assertNotIn('generation_answer_tokens', install_config.load(path)[0])
            # The plan renders the recorded bound with a model upstream.
            path.write_text(json.dumps(selected))
            rendered = []
            def run(command, **kwargs):
                rendered.append(json.loads(Path(command[-1]).read_text()))
            with patch.object(install, 'tools', return_value={'helm': 'helm'}), patch.object(install, 'run', side_effect=run), \
                 contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(install.main(['plan', '--config', str(path), '--model-upstream', str(upstream)]), 0)
            self.assertEqual(json.loads(rendered[0]['candidate']['configJSON'])['profile']['maxTokens'], 8192)
            self.assertEqual(rendered[0]['modelGateway']['benchmark']['completionTokens'], 8192)
            # Above 4096 needs a model upstream; the Codex path refuses before tools.
            with patch.object(install, 'tools') as tools:
                with self.assertRaisesRegex(ValueError, 'requires --model-upstream'):
                    install.main(['plan', '--config', str(path)])
                with self.assertRaisesRegex(ValueError, 'conflicts'):
                    install.main(['local', '--config', str(path), '--generation-answer-tokens', '4096'])
                tools.assert_not_called()
            # Status addresses the same scope without the plan/local-only option.
            with patch.object(install, 'tools', return_value={'helm': 'helm', 'kind': 'kind', 'kubectl': 'kubectl'}), \
                 patch.object(install, 'run', side_effect=ValueError('stop at cluster access')):
                with self.assertRaisesRegex(ValueError, 'stop at cluster access'):
                    install.main(['status', '--config', str(path)])
            invalid = [dict(candidate, generationAnswerTokens=4095), dict(candidate, generationAnswerTokens=8193),
                       dict(candidate, generationAnswerTokens=True), dict(candidate, generationAnswerTokens='8192'),
                       dict(candidate, generationAnswerTokens=8192.0), dict(self.fixture, generationAnswerTokens=4096)]
            for config in invalid:
                path.write_text(json.dumps(config))
                with self.subTest(config=config), self.assertRaises(ValueError):
                    install_config.load(path)
                # JSON Schema accepts 8192.0 as an integer; the parser rejects it.
                if not isinstance(config.get('generationAnswerTokens'), float):
                    with self.subTest(schema=config), self.assertRaises(jsonschema.ValidationError):
                        jsonschema.validate(config, schema)


if __name__ == '__main__':
    unittest.main()
