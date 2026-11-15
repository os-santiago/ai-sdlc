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

class ReviewInstallTests(unittest.TestCase):
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
