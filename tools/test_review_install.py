import argparse
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import candidate_install
import install
import install_config
from verify_review_install import require_review_disabled

class ReviewInstallTests(unittest.TestCase):
    def test_repair_opt_in_changes_only_generation_profile(self):
        lock_path = install.LOCAL / 'images.lock.json'
        evidence = json.loads(lock_path.with_name('images.evidence.json').read_text())
        evidence['images']['verifier'] = dict(evidence['images']['worker'], component='verifier')
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'images.lock.json'
            path.with_name('images.evidence.json').write_text(json.dumps(evidence))
            original = candidate_install.desired_values(install.ROOT, path, install.image_lock(lock_path), verification=True, review=True)
            repaired = candidate_install.desired_values(install.ROOT, path, install.image_lock(lock_path), verification=True, review=True, repair=True)
        before = json.loads(original['candidate']['configJSON'])
        after = json.loads(repaired['candidate']['configJSON'])
        self.assertNotIn('maxRepairs', before['profile'])
        self.assertEqual(after['profile'].pop('maxRepairs'), 1)
        self.assertEqual(after, before)
        self.assertEqual(original['review'], repaired['review'])
        self.assertNotIn('maxRepairs', json.loads(repaired['review']['configJSON'])['profile'])
        with self.assertRaises(ValueError):
            candidate_install.desired_values(install.ROOT, lock_path, {}, repair=True)

    def test_repair_config_is_explicit_and_requires_review(self):
        fixture = json.loads((install.ROOT / 'examples/homedir.local-install.json').read_text())
        fixture.update(profile='verification', independentReview=True)
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'config.json'
            path.write_text(json.dumps(fixture))
            original = install_config.load(path)[1]
            fixture['reviewRepair'] = True
            path.write_text(json.dumps(fixture))
            values, digest = install_config.load(path)
            self.assertIs(values['review_repair'], True)
            self.assertNotEqual(digest, original)
            for value in (1, 'true', None):
                fixture['reviewRepair'] = value
                path.write_text(json.dumps(fixture))
                with self.assertRaises(ValueError):
                    install_config.load(path)
            fixture.update(reviewRepair=True, independentReview=False)
            path.write_text(json.dumps(fixture))
            with self.assertRaises(ValueError):
                install_config.load(path)
        with patch.object(install, 'tools') as tools:
            with self.assertRaises(ValueError):
                install.main(['local', '--review-repair'])
            tools.assert_not_called()

    def test_disabled_inspection_rejects_hidden_review_credentials(self):
        base = {'containers': [{'name': 'verifier'}]}
        require_review_disabled(base)
        variants = [
            {'volumes': [{'name': 'review'}]},
            {'volumes': [{'name': 'renamed', 'secret': {'secretName': 'ai-sdlc-codex-auth'}}]},
            {'volumes': [{'name': 'renamed', 'projected': {'sources': [{'secret': {'name': 'ai-sdlc-review-model-client'}}]}}]},
            {'containers': [{'name': 'verifier', 'volumeMounts': [{'name': 'renamed', 'mountPath': '/var/run/ai-sdlc/review-model/token'}]}]},
            {'containers': [{'name': 'verifier', 'env': [{'name': 'TOKEN', 'valueFrom': {'secretKeyRef': {'name': 'ai-sdlc-review-model-client', 'key': 'token'}}}]}]},
            {'initContainers': [{'name': 'hidden', 'envFrom': [{'secretRef': {'name': 'ai-sdlc-codex-auth'}}]}]},
            {'ephemeralContainers': [{'name': 'hidden', 'volumeMounts': [{'name': 'model-auth', 'mountPath': '/elsewhere'}]}]},
        ]
        for variant in variants:
            with self.subTest(variant=variant), self.assertRaises(ValueError):
                require_review_disabled(dict(base, **variant))

    def test_review_configuration_is_bounded_and_credential_free(self):
        lock_path = install.LOCAL / 'images.lock.json'
        evidence = json.loads(lock_path.with_name('images.evidence.json').read_text())
        evidence['images']['verifier'] = dict(evidence['images']['worker'], component='verifier')
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'images.lock.json'
            path.with_name('images.evidence.json').write_text(json.dumps(evidence))
            values = candidate_install.desired_values(install.ROOT, path, install.image_lock(lock_path), verification=True, review=True)
        config = json.loads(values['review']['configJSON'])
        self.assertTrue(values['review']['enabled'])
        self.assertEqual(config['profile']['version'], 'ai-sdlc.reviewer-profile/v1')
        self.assertEqual((config['profile']['maxSteps'], config['profile']['maxSeconds']), (8, 180))
        self.assertEqual(set(config), {'version', 'profile', 'aDev', 'workerImage', 'mailboxImage'})
        with self.assertRaises(ValueError):
            candidate_install.desired_values(install.ROOT, lock_path, install.image_lock(lock_path), review=True)

    def test_option_is_explicit_and_part_of_config_digest(self):
        fixture = json.loads((install.ROOT / 'examples/homedir.local-install.json').read_text())
        fixture['profile'] = 'verification'
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'config.json'
            path.write_text(json.dumps(fixture))
            original = install_config.load(path)[1]
            fixture['independentReview'] = True
            path.write_text(json.dumps(fixture))
            config, digest = install_config.load(path)
            self.assertTrue(config['independent_review'])
            self.assertNotEqual(digest, original)
            args = argparse.Namespace(config=path, command='status', **{key: None for key in ('cluster', 'provider', 'images', 'repository', 'environment', 'profile', 'independent_review')})
            install_config.resolve(args)
            self.assertIsNone(args.profile)
            for value in ('true', 1, None):
                fixture['independentReview'] = value
                path.write_text(json.dumps(fixture))
                with self.assertRaises(ValueError):
                    install_config.load(path)
            fixture.update(profile='candidate', independentReview=True)
            path.write_text(json.dumps(fixture))
            with self.assertRaises(ValueError):
                install_config.load(path)

    def test_cli_rejects_wrong_mode_before_tools(self):
        with patch.object(install, 'tools') as tools:
            with self.assertRaises(ValueError):
                install.main(['local', '--independent-review'])
            tools.assert_not_called()

    def test_review_secret_is_separate_and_owned(self):
        with patch.object(install, 'owned_secret', return_value=None), patch.object(install, 'create_owned_secret') as create:
            candidate_install.review_credentials(['kubectl'], 'fixture-uid')
            self.assertEqual(create.call_args.args[1], 'ai-sdlc-review-model-client')
            self.assertEqual(create.call_args.args[3], 'fixture-uid')
        with patch.object(install, 'owned_secret', return_value={'token': 'x' * 48}), patch.object(install, 'create_owned_secret') as create:
            candidate_install.review_credentials(['kubectl'], 'fixture-uid')
            create.assert_not_called()

if __name__ == '__main__':
    unittest.main()
