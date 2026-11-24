import contextlib
import copy
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import candidate_install
import install


class CandidateInstallTests(unittest.TestCase):
    def test_existing_tool_budget_gate_precedes_runtime_effects(self):
        class PassedBudgetGate(Exception):
            pass
        lock_path = install.LOCAL / 'images.lock.json'
        desired = candidate_install.desired_values(install.ROOT, lock_path, install.image_lock(lock_path))
        original = json.loads(desired['candidate']['configJSON'])
        old = json.loads(json.dumps(original))
        old['profile']['maxSteps'] = 16
        malformed = [None, '{', [], {'profile': {'maxSteps': 24}},
                     dict(original, profile=dict(original['profile'], maxSteps=True))]
        cases = [('new', None, True), ('same', json.dumps(original), True),
                 ('changed', json.dumps(old), False), ('api-error', None, False),
                 ('api-timeout', None, False), ('api-oversize', None, False)]
        cases += [('malformed-' + str(n), json.dumps(value) if value != '{' else value, False)
                  for n, value in enumerate(malformed)]
        cases.append(('duplicate', json.dumps(original).replace('"maxSteps": 24', '"maxSteps": 16, "maxSteps": 24'), False))
        for profile in ('candidate', 'verification'):
            for label, config, allowed in cases:
                with self.subTest(profile=profile, case=label), tempfile.TemporaryDirectory() as directory:
                    state = Path(directory) / 'ai-sdlc' / 'budget-fixture'
                    state.mkdir(parents=True)
                    (state / 'ownership.json').write_text(json.dumps({'cluster': 'budget-fixture', 'provider': 'podman', 'clusterUID': 'fixture-uid'}))
                    commands = []
                    def run(command, **kwargs):
                        argv = [str(value) for value in command]
                        commands.append(argv)
                        if argv[:3] == ['kind', 'get', 'clusters']:
                            return subprocess.CompletedProcess(argv, 0, 'budget-fixture\n')
                        if argv[:3] == ['kind', 'export', 'kubeconfig']:
                            (state / 'kubeconfig').write_text('fixture only\n')
                        if 'kube-system' in argv and 'get' in argv:
                            return subprocess.CompletedProcess(argv, 0, 'fixture-uid')
                        if 'ai-sdlc-execution' in argv:
                            self.assertIn('--ignore-not-found', argv)
                            self.assertIn('--request-timeout=10s', argv)
                            self.assertEqual(kwargs['timeout'], 15)
                            if label == 'api-error':
                                raise subprocess.CalledProcessError(1, argv)
                            if label == 'api-timeout':
                                raise subprocess.TimeoutExpired(argv, 15)
                            if label == 'api-oversize':
                                return subprocess.CompletedProcess(argv, 0, 'x' * 1048577)
                            raw = '' if label == 'new' else json.dumps({'data': {'config.json': config}})
                            return subprocess.CompletedProcess(argv, 0, raw)
                        if argv[0] == 'kubectl' and 'wait' in argv:
                            raise PassedBudgetGate()
                        self.assertFalse(argv[0] == 'kubectl' and any(v in argv for v in ('apply', 'create', 'patch', 'delete')))
                        self.assertFalse(argv[0] == 'helm' and 'upgrade' in argv)
                        return subprocess.CompletedProcess(argv, 0, '')
                    with patch.dict(os.environ, XDG_STATE_HOME=directory), \
                         patch.object(install, 'tools', return_value={name: name for name in ('helm', 'kind', 'kubectl')}), \
                         patch.object(install.shutil, 'which', return_value='/fixture/provider'), \
                         patch.object(install, 'host_prerequisites'), patch.object(install, 'run', side_effect=run), \
                         patch.object(install, 'configure_identities') as identities, \
                         patch.object(candidate_install, 'model_credentials') as credentials:
                        expected = (PassedBudgetGate if allowed else subprocess.CalledProcessError if label == 'api-error'
                                    else subprocess.TimeoutExpired if label == 'api-timeout' else ValueError)
                        with self.assertRaises(expected):
                            install.main(['local', '--cluster', 'budget-fixture', '--provider', 'podman', '--profile', profile])
                        identities.assert_not_called()
                        credentials.assert_not_called()
                    self.assertTrue(any('ai-sdlc-execution' in argv for argv in commands))

    def test_reviewed_identity_pair_and_bounded_profile(self):
        lock_path = install.LOCAL / 'images.lock.json'
        lock = install.image_lock(lock_path)
        values = candidate_install.desired_values(install.ROOT, lock_path, lock)
        config = json.loads(values['candidate']['configJSON'])
        self.assertEqual(config['placement']['Namespace'], 'ai-sdlc-executors')
        self.assertTrue(config['placement']['WorkerImage'].endswith(lock['components']['worker']['digest']))
        self.assertEqual(config['profile']['model'], 'codex-cli')
        self.assertEqual(config['profile']['maxSteps'], 24)
        self.assertEqual(config['profile']['maxSeconds'], 600)
        self.assertEqual(config['profile']['maxTokens'], 4096)
        self.assertEqual(config['model']['Limits']['Calls'], 16)
        self.assertEqual(config['model']['Limits']['InputBytes'], 2097152)
        self.assertEqual(config['model']['Limits']['OutputBytes'], 4194304)
        self.assertEqual(config['model']['ResponseLimit'], 262144)
        for forbidden in ('token', 'password', 'credential', 'command', 'shell'):
            self.assertNotIn('"' + forbidden + '"', values['candidate']['configJSON'])

    def test_sandbox_runtime_class_is_written_only_when_not_default(self):
        lock_path = install.LOCAL / 'images.lock.json'
        lock = install.image_lock(lock_path)
        default = candidate_install.desired_values(install.ROOT, lock_path, lock, verification=True, review=True)
        explicit = candidate_install.desired_values(install.ROOT, lock_path, lock, verification=True, review=True,
                                                    runtime_class='ai-sdlc-offline')
        # Default installations keep byte-identical configuration documents.
        self.assertEqual(default, explicit)
        self.assertNotIn('sandbox', default)
        self.assertNotIn('RuntimeClass', default['candidate']['configJSON'])
        self.assertNotIn('runtimeClass', default['review']['configJSON'])
        kata = candidate_install.desired_values(install.ROOT, lock_path, lock, verification=True, review=True, runtime_class='kata')
        self.assertEqual(kata['sandbox'], {'runtimeClassName': 'kata'})
        self.assertEqual(json.loads(kata['candidate']['configJSON'])['placement']['RuntimeClass'], 'kata')
        self.assertEqual(json.loads(kata['review']['configJSON'])['runtimeClass'], 'kata')
        for bad in ('', 'Kata', 'kata/../runc', '-kata', 'a' * 254, None):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                candidate_install.desired_values(install.ROOT, lock_path, lock, runtime_class=bad)

    def test_reinstall_never_switches_the_installed_runtime_class_implicitly(self):
        def installed(config):
            raw = '' if config is None else json.dumps({'data': {'config.json': json.dumps(config)}})
            return patch.object(install, 'run', return_value=subprocess.CompletedProcess([], 0, raw))
        kata = {'placement': {'Namespace': 'ai-sdlc-executors', 'RuntimeClass': 'kata'}}
        default = {'placement': {'Namespace': 'ai-sdlc-executors'}}
        for config, explicit in ((None, None), (default, None), (default, 'kata'), (kata, 'kata'), (kata, 'gvisor'), (kata, 'ai-sdlc-offline')):
            with self.subTest(config=config, explicit=explicit), installed(config):
                candidate_install.require_runtime_class(['kubectl'], explicit)
        # The runtime reads the field names case-insensitively; so does the guard.
        for spelled in (kata, {'placement': {'runtimeClass': 'kata'}}, {'Placement': {'RUNTIMECLASS': 'kata'}}):
            with self.subTest(spelled=spelled), installed(spelled), self.assertRaisesRegex(ValueError, 'pass --runtime-class kata'):
                candidate_install.require_runtime_class(['kubectl'], None)
        for bad in ([], {'placement': []}, {'placement': {'RuntimeClass': 7}},
                    {'placement': {'RuntimeClass': 'kata', 'runtimeClass': 'kata'}}, {'placement': {}, 'Placement': {}}):
            with self.subTest(bad=bad), installed(bad), self.assertRaisesRegex(ValueError, 'malformed'):
                candidate_install.require_runtime_class(['kubectl'], None)

    def test_generation_budget_defaults_are_the_reviewed_profile(self):
        lock_path = install.LOCAL / 'images.lock.json'
        lock = install.image_lock(lock_path)
        self.assertEqual(candidate_install.generation_budget(), (600, 24))
        for kwargs in ({}, {'verification': True, 'review': True, 'repair': True}):
            default = candidate_install.desired_values(install.ROOT, lock_path, lock, **kwargs)
            explicit = candidate_install.desired_values(install.ROOT, lock_path, lock, generation_seconds=600,
                                                        generation_steps=24, **kwargs)
            # Byte-identical configuration, so default installations keep their digests.
            self.assertEqual(json.dumps(default, sort_keys=True), json.dumps(explicit, sort_keys=True))
            profile = json.loads(default['candidate']['configJSON'])['profile']
            self.assertEqual(set(profile) - {'maxRepairs'},
                             {'version', 'model', 'maxSteps', 'maxSeconds', 'maxTokens', 'aDevSHA256'})
            self.assertEqual((profile['maxSeconds'], profile['maxSteps']), (600, 24))

    def test_selected_generation_budget_is_recorded_and_bounded(self):
        lock_path = install.LOCAL / 'images.lock.json'
        lock = install.image_lock(lock_path)
        values = candidate_install.desired_values(install.ROOT, lock_path, lock, verification=True, review=True,
                                                  generation_seconds=900, generation_steps=32, model_upstream=True)
        config = json.loads(values['candidate']['configJSON'])
        self.assertEqual((config['profile']['maxSeconds'], config['profile']['maxSteps']), (900, 32))
        # The bundled Codex path keeps 24 steps and its 16-call provider quota:
        # more steps than its ledger can reach are refused, not recorded.
        codex = json.loads(candidate_install.desired_values(install.ROOT, lock_path, lock, generation_seconds=900)['candidate']['configJSON'])
        self.assertEqual((codex['profile']['maxSeconds'], codex['profile']['maxSteps']), (900, 24))
        self.assertEqual(codex['model'], {'Limits': {'Calls': 16, 'InputBytes': 2097152, 'OutputBytes': 4194304},
                                          'ResponseLimit': 262144})
        for steps in (25, 32):
            with self.subTest(codex_steps=steps), self.assertRaisesRegex(ValueError, 'requires --model-upstream'):
                candidate_install.desired_values(install.ROOT, lock_path, lock, generation_steps=steps)
        review = json.loads(values['review']['configJSON'])['profile']
        self.assertEqual((review['maxSeconds'], review['maxSteps']), (180, 8))
        for seconds, steps in ((599, 24), (901, 24), (600, 23), (600, 33), (True, 24), (600, False),
                               ('900', 24), (600, 24.0), (0, 24), (-600, 24)):
            with self.subTest(seconds=seconds, steps=steps), self.assertRaises(ValueError):
                candidate_install.desired_values(install.ROOT, lock_path, lock, generation_seconds=seconds,
                                                 generation_steps=steps, model_upstream=True)

    def test_generation_budget_option_is_validated_before_tools(self):
        base = ['plan', '--profile', 'candidate']
        invalid = [base + ['--generation-seconds', '599'], base + ['--generation-seconds', '901'],
                   base + ['--generation-steps', '23'], base + ['--generation-steps', '33'],
                   base + ['--generation-steps', '25'], base + ['--generation-seconds', '900', '--generation-steps', '32'],
                   ['plan', '--profile', 'shadow', '--generation-seconds', '900'],
                   ['plan', '--generation-steps', '32'],
                   ['status', '--generation-seconds', '900'], ['credentials', '--generation-steps', '32',
                                                                '--github-auth', 'gh-cli']]
        with patch.object(install, 'tools') as tools:
            for argv in invalid:
                with self.subTest(argv=argv), self.assertRaises(ValueError):
                    install.main(argv)
            tools.assert_not_called()
        with patch.object(install, 'tools') as tools, patch('sys.stderr'), self.assertRaises(SystemExit):
            install.main(base + ['--generation-seconds', 'slow'])
        tools.assert_not_called()

    def test_reconciliation_keeps_the_recorded_generation_time(self):
        lock_path = install.LOCAL / 'images.lock.json'
        lock = install.image_lock(lock_path)
        recorded = candidate_install.desired_values(install.ROOT, lock_path, lock, generation_seconds=900)
        default = candidate_install.desired_values(install.ROOT, lock_path, lock)
        configmap = json.dumps({'data': {'config.json': recorded['candidate']['configJSON']}})
        with patch.object(install, 'run', return_value=subprocess.CompletedProcess([], 0, configmap)):
            candidate_install.require_unchanged_tool_budget(['kubectl'], recorded)
            # Omitting --generation-seconds would reset 900 s to 600 s: refused.
            with self.assertRaisesRegex(ValueError, 'generation time budget differs.*requires a reinstallation'):
                candidate_install.require_unchanged_tool_budget(['kubectl'], default)
        stepped = candidate_install.desired_values(install.ROOT, lock_path, lock, generation_steps=32, model_upstream=True)
        configmap = json.dumps({'data': {'config.json': stepped['candidate']['configJSON']}})
        with patch.object(install, 'run', return_value=subprocess.CompletedProcess([], 0, configmap)):
            with self.assertRaisesRegex(ValueError, 'tool budget differs.*requires a reinstallation'):
                candidate_install.require_unchanged_tool_budget(['kubectl'], default)
        for bad in (0, 901, True, '600', None):
            config = json.loads(default['candidate']['configJSON'])
            config['profile']['maxSeconds'] = bad
            configmap = json.dumps({'data': {'config.json': json.dumps(config)}})
            with self.subTest(maxSeconds=bad), patch.object(install, 'run', return_value=subprocess.CompletedProcess([], 0, configmap)), \
                 self.assertRaisesRegex(ValueError, 'malformed'):
                candidate_install.require_unchanged_tool_budget(['kubectl'], default)

    def test_answer_token_default_keeps_the_reviewed_configuration(self):
        lock_path = install.LOCAL / 'images.lock.json'
        lock = install.image_lock(lock_path)
        self.assertEqual(candidate_install.answer_tokens(), 4096)
        for kwargs in ({}, {'verification': True, 'review': True, 'repair': True},
                       {'verification': True, 'review': True, 'model_upstream': True, 'generation_steps': 32}):
            default = candidate_install.desired_values(install.ROOT, lock_path, lock, **kwargs)
            explicit = candidate_install.desired_values(install.ROOT, lock_path, lock, generation_answer_tokens=4096, **kwargs)
            # Byte-identical configuration, so default installations keep their digests.
            self.assertEqual(json.dumps(default, sort_keys=True), json.dumps(explicit, sort_keys=True))
            self.assertEqual(json.loads(default['candidate']['configJSON'])['profile']['maxTokens'], 4096)

    def test_selected_answer_tokens_are_recorded_and_bounded(self):
        lock_path = install.LOCAL / 'images.lock.json'
        lock = install.image_lock(lock_path)
        default = candidate_install.desired_values(install.ROOT, lock_path, lock, verification=True, review=True,
                                                   model_upstream=True)
        values = candidate_install.desired_values(install.ROOT, lock_path, lock, verification=True, review=True,
                                                  model_upstream=True, generation_answer_tokens=8192)
        config = json.loads(values['candidate']['configJSON'])
        self.assertEqual(config['profile']['maxTokens'], 8192)
        # Only the generator's answer bound changes: reviewers keep their default.
        changed = copy.deepcopy(config)
        changed['profile']['maxTokens'] = 4096
        self.assertEqual(changed, json.loads(default['candidate']['configJSON']))
        self.assertEqual(values['review'], default['review'])
        self.assertEqual(json.loads(values['review']['configJSON'])['profile']['maxTokens'], 4096)
        # The bundled Codex adapter never applies the bound: above 4096 is refused there.
        for tokens in (4097, 8192):
            with self.subTest(codex_tokens=tokens), self.assertRaisesRegex(ValueError, 'requires --model-upstream'):
                candidate_install.desired_values(install.ROOT, lock_path, lock, generation_answer_tokens=tokens)
        for tokens in (4095, 8193, 0, -4096, True, '8192', 8192.0):
            with self.subTest(tokens=tokens), self.assertRaises(ValueError):
                candidate_install.desired_values(install.ROOT, lock_path, lock, model_upstream=True,
                                                 generation_answer_tokens=tokens)

    def test_answer_token_option_is_validated_before_tools(self):
        base = ['plan', '--profile', 'candidate']
        invalid = [base + ['--generation-answer-tokens', '4095'], base + ['--generation-answer-tokens', '8193'],
                   base + ['--generation-answer-tokens', '8192'],
                   ['plan', '--profile', 'shadow', '--generation-answer-tokens', '4096'],
                   ['plan', '--generation-answer-tokens', '4096'],
                   ['status', '--generation-answer-tokens', '4096'],
                   ['credentials', '--generation-answer-tokens', '4096', '--github-auth', 'gh-cli']]
        with patch.object(install, 'tools') as tools:
            for argv in invalid:
                with self.subTest(argv=argv), self.assertRaises(ValueError):
                    install.main(argv)
            tools.assert_not_called()
        with patch.object(install, 'tools') as tools, patch('sys.stderr'), self.assertRaises(SystemExit):
            install.main(base + ['--generation-answer-tokens', 'long'])
        tools.assert_not_called()

    def test_selected_answer_tokens_reach_the_rendered_plan(self):
        with tempfile.TemporaryDirectory() as directory:
            upstream = Path(directory) / 'upstream.json'
            upstream.write_text(json.dumps({'adapter': 'openai', 'url': 'http://10.89.0.1:18090/v1/chat/completions',
                                            'model': 'local-model', 'cidr': '10.89.0.1/32', 'port': 18090,
                                            'seconds': 300, 'directAnswers': True, 'callSeconds': 330,
                                            'reviewCallSeconds': 170, 'credentialSecret': ''}))
            rendered = []
            def run(command, **kwargs):
                rendered.append(json.loads(Path(command[-1]).read_text()))
            with patch.object(install, 'tools', return_value={'helm': 'helm'}), patch.object(install, 'run', side_effect=run), \
                 contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(install.main(['plan', '--profile', 'candidate', '--model-upstream', str(upstream),
                                               '--generation-answer-tokens', '8192']), 0)
        self.assertEqual(json.loads(rendered[0]['candidate']['configJSON'])['profile']['maxTokens'], 8192)
        self.assertEqual(rendered[0]['modelGateway']['benchmark'], {'promptTokens': 16384, 'completionTokens': 8192})

    def test_reconciliation_keeps_the_recorded_answer_tokens(self):
        lock_path = install.LOCAL / 'images.lock.json'
        lock = install.image_lock(lock_path)
        recorded = candidate_install.desired_values(install.ROOT, lock_path, lock, model_upstream=True,
                                                    generation_answer_tokens=8192)
        default = candidate_install.desired_values(install.ROOT, lock_path, lock, model_upstream=True)
        configmap = json.dumps({'data': {'config.json': recorded['candidate']['configJSON']}})
        with patch.object(install, 'run', return_value=subprocess.CompletedProcess([], 0, configmap)):
            candidate_install.require_unchanged_tool_budget(['kubectl'], recorded)
            # Omitting --generation-answer-tokens would reset 8192 to 4096: refused.
            with self.assertRaisesRegex(ValueError, 'answer-token bound differs.*requires a reinstallation'):
                candidate_install.require_unchanged_tool_budget(['kubectl'], default)
        configmap = json.dumps({'data': {'config.json': default['candidate']['configJSON']}})
        with patch.object(install, 'run', return_value=subprocess.CompletedProcess([], 0, configmap)):
            with self.assertRaisesRegex(ValueError, 'answer-token bound differs.*requires a reinstallation'):
                candidate_install.require_unchanged_tool_budget(['kubectl'], recorded)
        for bad in (0, 8193, True, '4096', None):
            config = json.loads(default['candidate']['configJSON'])
            config['profile']['maxTokens'] = bad
            configmap = json.dumps({'data': {'config.json': json.dumps(config)}})
            with self.subTest(maxTokens=bad), patch.object(install, 'run', return_value=subprocess.CompletedProcess([], 0, configmap)), \
                 self.assertRaisesRegex(ValueError, 'malformed'):
                candidate_install.require_unchanged_tool_budget(['kubectl'], default)

    def test_mismatched_publication_cannot_supply_executor(self):
        lock = install.image_lock(install.LOCAL / 'images.lock.json')
        evidence = json.loads((install.LOCAL / 'images.evidence.json').read_text())
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'images.lock.json'
            for field, value in (('revision', '0' * 40), ('repository', 'untrusted/source')):
                bad = dict(evidence, **{field: value})
                path.with_name('images.evidence.json').write_text(json.dumps(bad))
                with self.assertRaises(ValueError):
                    candidate_install.desired_values(install.ROOT, path, lock)

    def test_model_import_requires_explicit_candidate_installation(self):
        with patch.object(install, 'tools') as tools:
            for args in (['local', '--codex-auth-file', '/private/auth.json'],
                         ['plan', '--profile', 'candidate', '--codex-auth-file', '/private/auth.json']):
                with self.assertRaises(ValueError):
                    install.main(args)
            tools.assert_not_called()

    def test_credential_refresh_returns_public_revision_for_new_pod(self):
        import subprocess
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / 'auth.json'
            source.write_text(json.dumps({'test_identity': 'B'}))
            writes = []
            def run(command, **kwargs):
                if 'patch' in command:
                    writes.append(json.loads(kwargs['input'])['stringData']['auth.json'])
                return subprocess.CompletedProcess(command, 0, json.dumps({'metadata': {'resourceVersion': '102'}}))
            def owned(_kubectl, name, _uid):
                return {'auth.json': '{"test_identity":"A"}'} if name == 'ai-sdlc-codex-auth' else {'token': 'x' * 48}
            with patch.object(install, 'owned_secret', side_effect=owned), patch.object(install, 'run', side_effect=run):
                revision = candidate_install.model_credentials(['kubectl'], 'fixture', source, True)
            self.assertEqual(revision, '102')
            self.assertEqual([json.loads(value)['test_identity'] for value in writes], ['B'])

    def test_verification_requires_exact_published_identity(self):
        lock_path = install.LOCAL / 'images.lock.json'
        lock = install.image_lock(lock_path)
        evidence = json.loads(lock_path.with_name('images.evidence.json').read_text())
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'images.lock.json'
            evidence_path = path.with_name('images.evidence.json')
            verifier = evidence['images'].pop('verifier')
            evidence_path.write_text(json.dumps(evidence))
            with self.assertRaises(ValueError):
                candidate_install.desired_values(install.ROOT, path, lock, verification=True)
            evidence['images']['verifier'] = verifier
            evidence_path.write_text(json.dumps(evidence))
            result = candidate_install.desired_values(install.ROOT, path, lock, verification=True)
            self.assertEqual(result['verifier']['digest'], verifier['digest'])
            evidence['images']['verifier']['revision'] = '0' * 40
            evidence_path.write_text(json.dumps(evidence))
            with self.assertRaises(ValueError):
                candidate_install.desired_values(install.ROOT, path, lock, verification=True)
