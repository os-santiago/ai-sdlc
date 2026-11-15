"""Failure-path checks for the local validation driver; no daemon required."""
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from validate_runtime import OWNER_LABEL, REQUIRED_TESTS, Run, ValidationError, test_evidence


class ValidationDriverTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)
        self.run = Run('podman', self.path, self.path, self.path / 'cache', 60, {})

    def test_skipped_database_tests_cannot_establish_success(self):
        log = self.path / 'test.log'
        events = [{'Action': 'skip', 'Package': 'example/runtime/k8s/' + package, 'Test': name}
                  for package, name in REQUIRED_TESTS]
        log.write_text('\n'.join(json.dumps(event) for event in events))
        self.assertEqual(len(test_evidence(log)['missingDatabaseProof']), len(REQUIRED_TESTS))
        events[0]['Action'] = 'pass'
        log.write_text('\n'.join(json.dumps(event) for event in events))
        self.assertEqual(len(test_evidence(log)['missingDatabaseProof']), len(REQUIRED_TESTS) - 1)

    def test_cleanup_refuses_unowned_or_malformed_objects(self):
        for data in ([{'Id': 'a' * 64, 'Config': {'Labels': {OWNER_LABEL: 'other'}}}],
                     [{'Id': 'a' * 64, 'Config': {'Labels': None}}], {}):
            with self.subTest(data=data), patch('validate_runtime.subprocess.run') as call:
                call.return_value = subprocess.CompletedProcess([], 0, json.dumps(data))
                self.run.report['cleanup'] = []
                self.run.cleanup()
                self.assertTrue(all(item['status'] == 'refused-unowned' for item in self.run.report['cleanup']))
                self.assertFalse(any('rm' in item.args[0] for item in call.call_args_list))

    def test_cleanup_deletes_by_observed_id_not_reusable_name(self):
        identity = 'a' * 64
        inspected = json.dumps([{'Id': identity, 'Config': {'Labels': {OWNER_LABEL: self.run.identity}}}])
        with patch('validate_runtime.subprocess.run') as call:
            call.return_value = subprocess.CompletedProcess([], 0, inspected)
            self.run.cleanup()
            removals = [item.args[0] for item in call.call_args_list if 'rm' in item.args[0]]
            self.assertEqual(removals, [['podman', 'rm', '-f', '-v', identity]] * 2)

    def test_inspection_failure_is_not_assumed_absence(self):
        with patch('validate_runtime.subprocess.run') as call:
            call.side_effect = subprocess.CalledProcessError(1, 'daemon-unavailable')
            self.run.cleanup()
            self.assertTrue(all(item['status'] == 'failed' for item in self.run.report['cleanup']))

    def test_timeout_becomes_bounded_failure(self):
        with patch('validate_runtime.subprocess.run', side_effect=subprocess.TimeoutExpired('fixture', 1)):
            with self.assertRaisesRegex(ValidationError, 'deadline'):
                self.run.call(['version'])

    def test_failed_stage_cleans_resources_without_writing_credentials(self):
        (self.path / '.git/objects').mkdir(parents=True)
        (self.path / '.git/HEAD').write_text('ref: refs/heads/main')
        def command(args, **kwargs):
            if '--git-common-dir' in args:
                return subprocess.CompletedProcess(args, 0, str(self.path / '.git'))
            if args[0] == 'git':
                return subprocess.CompletedProcess(args, 0, 'a' * 40 if 'rev-parse' in args else b'')
            if 'inspect' in args:
                return subprocess.CompletedProcess(args, 1, '')
            if 'ps' in args:
                return subprocess.CompletedProcess(args, 0, '')
            raise AssertionError('Unexpected subprocess')
        self.run.lock = {'platform': 'linux/amd64', 'postgres': 'fixture'}
        def fail_start(args, **kwargs):
            self.assertNotIn('fixture-value-must-not-be-recorded', ' '.join(args))
            self.assertNotIn('--env-file', args)
            self.assertIn('POSTGRES_PASSWORD', args)
            self.assertEqual(self.run.environment['POSTGRES_PASSWORD'], 'fixture-value-must-not-be-recorded')
            self.assertFalse((self.path / 'database.env').exists())
            raise ValidationError('simulated start failure')
        with patch('validate_runtime.subprocess.run', side_effect=command), \
                patch.object(self.run, 'call', side_effect=fail_start), \
                patch('validate_runtime.secrets.token_urlsafe', return_value='fixture-value-must-not-be-recorded'):
            self.assertEqual(self.run.execute(), 1)
        self.assertFalse((self.path / 'database.env').exists())
        result = json.loads((self.path / 'report.json').read_text())
        self.assertEqual(result['status'], 'failed')
        self.assertEqual(result['stages'][0]['status'], 'failed')
        self.assertEqual([row['status'] for row in result['cleanup']], ['absent', 'absent'])
        self.assertNotIn('POSTGRES_PASSWORD', json.dumps(result))
        self.assertNotIn('fixture-value-must-not-be-recorded', json.dumps(result))
        self.assertIsNone(self.run.environment)


if __name__ == '__main__':
    unittest.main()
