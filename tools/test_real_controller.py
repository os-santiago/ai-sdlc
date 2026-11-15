import contextlib
import io
import subprocess
import sys
import unittest
from unittest.mock import Mock, patch

import verify_real_controller as cli
from verify_lifecycle_model import verify


class RealControllerTests(unittest.TestCase):
    def test_model_connection_cannot_override_fixture_inputs(self):
        invalid = [None, {'Endpoint': 'http://127.0.0.1:8787/v1/chat/completions', 'Token': 'a'*48, 'Placement': {}},
                   {'Endpoint': 'https://example.com/v1/chat/completions', 'Token': 'a'*48},
                   {'Endpoint': 'http://127.0.0.1:8787/v1/chat/completions?unsafe=1', 'Token': 'a'*48},
                   {'Endpoint': 'http://127.0.0.1:8787/v1/chat/completions', 'Token': 'short'}]
        for value in invalid:
            with self.subTest(value=value), self.assertRaises(ValueError):
                verify([], '', '', '', {}, 'real-model', value)
        with self.assertRaises(ValueError):
            verify([], '', '', '', {}, 'success', {})

    def test_forced_adapter_cleanup_cannot_emit_success(self):
        args = ['fixture']
        for name in ('kubectl', 'kubeconfig', 'worker-image', 'mailbox-image', 'test-binary', 'source-repository', 'source-sha'):
            args += ['--'+name, 'fixture']
        process = Mock()
        process.poll.return_value = None
        process.wait.side_effect = [subprocess.TimeoutExpired('owned-wrapper', 5), -9]
        output = io.StringIO()
        with patch.object(sys, 'argv', args), patch.object(cli, 'source_bundle', return_value={}), \
             patch.object(cli.subprocess, 'Popen', return_value=process), \
             patch.object(cli, 'verify', return_value={'status': 'passed'}), \
             patch.object(cli.urllib.request, 'urlopen') as opened, contextlib.redirect_stdout(output):
            opened.return_value.__enter__.return_value.status = 200
            with self.assertRaisesRegex(RuntimeError, 'forced termination'):
                cli.main()
        self.assertEqual(output.getvalue(), '')
        process.kill.assert_called_once()


if __name__ == '__main__':
    unittest.main()
