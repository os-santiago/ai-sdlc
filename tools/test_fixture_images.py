"""Fixture image aliases must converge without changing content or hiding failures."""
import json
import subprocess
import unittest
from unittest.mock import patch

import fixture_images


class AliasTests(unittest.TestCase):
    def run_case(self, *, delay=False, tag_failure=False, mismatch=False, wrong_target=False, existing=False, never=False):
        digest = 'sha256:' + 'a' * 64
        tag = 'localhost/ai-sdlc-worker:fixture'
        reference = tag.split(':')[0] + '@' + digest
        calls = []
        observations = 0
        registered = existing

        def run(command, **kwargs):
            nonlocal observations, registered
            calls.append(command)
            if 'crictl' in command:
                if command[-1] == reference:
                    observations += 1
                    if never or (delay and observations == 1):
                        raise subprocess.CalledProcessError(1, command, stderr=b'not visible')
                status = {'id': 'wrong' if mismatch and command[-1] == reference else 'original',
                          'repoDigests': [reference]}
                return subprocess.CompletedProcess(command, 0, json.dumps({'status': status}).encode())
            if 'ls' in command:
                name = command[-1].removeprefix('name==')
                target = 'sha256:' + 'b' * 64 if wrong_target else digest
                rows = f'{name} media {target} 1B linux/amd64 -\n' if name == tag or registered else ''
                return subprocess.CompletedProcess(command, 0, 'REF TYPE DIGEST SIZE PLATFORMS LABELS\n' + rows)
            self.assertIn('--local', command)
            self.assertNotIn('--force', command)
            registered = True
            return subprocess.CompletedProcess(command, 1 if tag_failure else 0, b'', b'registration rejected')

        clock = iter([0, 0, 11] if never else [0, 0, 1, 1, 1])
        with patch.object(fixture_images.subprocess, 'run', side_effect=run), patch.object(fixture_images.time, 'sleep'), patch.object(fixture_images.time, 'monotonic', side_effect=lambda: next(clock, 11 if never else 1)):
            result = fixture_images.register_digest('podman', 'ai-sdlc-test-control-plane', tag)
        return result, calls

    def test_waits_for_cri_after_single_local_registration(self):
        _, calls = self.run_case(delay=True)
        self.assertEqual(sum('tag' in call for call in calls), 1)

    def test_existing_alias_is_observed_without_retag(self):
        _, calls = self.run_case(existing=True)
        self.assertFalse(any('tag' in call for call in calls))

    def test_registration_failure_is_terminal_with_diagnostic(self):
        with self.assertRaisesRegex(ValueError, 'registration rejected'):
            self.run_case(tag_failure=True)

    def test_different_descriptor_is_terminal(self):
        with self.assertRaisesRegex(ValueError, 'descriptor'):
            self.run_case(wrong_target=True)

    def test_cri_identity_mismatch_is_terminal(self):
        with self.assertRaisesRegex(ValueError, 'does not resolve'):
            self.run_case(mismatch=True)

    def test_cri_visibility_has_deadline(self):
        with self.assertRaisesRegex(ValueError, 'not visible'):
            self.run_case(never=True)


if __name__ == '__main__':
    unittest.main()
