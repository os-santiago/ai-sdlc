import os
import unittest
from unittest.mock import patch

from model_fixture_environment import provider_environment
import verify_review_flow


class ProviderEnvironmentTests(unittest.TestCase):
    def test_source_credentials_do_not_reach_actual_provider_launch(self):
        ambient = {'PATH': '/usr/bin', 'HOME': '/fixture', 'CODEX_HOME': '/fixture/codex',
                   'AI_SDLC_TEST_SOURCE_TOKEN': 'private-read-token',
                   'AI_SDLC_TEST_DATABASE_URL': 'private-database',
                   'AI_SDLC_REVIEW_CONTROL': '/private/control', 'GH_TOKEN': 'private-gh',
                   'GITHUB_TOKEN': 'private-actions', 'KUBECONFIG': '/private/cluster'}
        with patch.dict(os.environ, ambient, clear=True):
            env = provider_environment(1234, 'adapter-token')
            self.assertEqual(set(env), {'PATH', 'HOME', 'CODEX_HOME', 'CODEX_WRAPPER_PORT', 'CODEX_WRAPPER_TOKEN'})
            self.assertEqual(os.environ['AI_SDLC_TEST_SOURCE_TOKEN'], 'private-read-token')
            with patch.object(verify_review_flow.subprocess, 'Popen', side_effect=RuntimeError('captured')) as launch:
                with self.assertRaisesRegex(RuntimeError, 'captured'):
                    with verify_review_flow.owned_provider('real-model'):
                        self.fail('provider unexpectedly started')
            actual = launch.call_args.kwargs['env']
            self.assertEqual(set(actual), set(env))
            self.assertEqual(actual['CODEX_HOME'], '/fixture/codex')


if __name__ == '__main__':
    unittest.main()
