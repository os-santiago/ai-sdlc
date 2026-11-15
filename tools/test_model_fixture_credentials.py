"""Only intended model credentials may reach the completion adapter."""
import os
import unittest
from unittest.mock import patch
from model_fixture_environment import provider_environment

class ProviderCredentialsTest(unittest.TestCase):
    def test_model_keys_retained_and_operational_credentials_excluded(self):
        model = {'OPENAI_API_KEY': 'fixture-model-key', 'CODEX_API_KEY': 'fixture-codex-key'}
        forbidden = {'GH_TOKEN': 'fixture-forge', 'AI_SDLC_TEST_SOURCE_TOKEN': 'fixture-source',
                     'DATABASE_URL': 'fixture-db', 'KUBECONFIG': 'fixture-cluster',
                     'CODEX_WRAPPER_TOKEN': 'untrusted-inherited-token'}
        with patch.dict(os.environ, dict(model, **forbidden), clear=True):
            env = provider_environment(4567, 'owned-fixture-token')
        for key, value in model.items():
            self.assertEqual(env[key], value)
        for key in forbidden:
            if key != 'CODEX_WRAPPER_TOKEN':
                self.assertNotIn(key, env)
        self.assertEqual(env['CODEX_WRAPPER_TOKEN'], 'owned-fixture-token')
