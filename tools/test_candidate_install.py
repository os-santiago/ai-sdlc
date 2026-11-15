import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import candidate_install
import install


class CandidateInstallTests(unittest.TestCase):
    def test_reviewed_identity_pair_and_bounded_profile(self):
        lock_path = install.LOCAL / 'images.lock.json'
        lock = install.image_lock(lock_path)
        values = candidate_install.desired_values(install.ROOT, lock_path, lock)
        config = json.loads(values['candidate']['configJSON'])
        self.assertEqual(config['placement']['Namespace'], 'ai-sdlc-executors')
        self.assertTrue(config['placement']['WorkerImage'].endswith(lock['components']['worker']['digest']))
        self.assertEqual(config['profile']['model'], 'codex-cli')
        self.assertEqual(config['model']['Limits']['Calls'], 16)
        for forbidden in ('token', 'password', 'credential', 'command', 'shell'):
            self.assertNotIn('"' + forbidden + '"', values['candidate']['configJSON'])

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
