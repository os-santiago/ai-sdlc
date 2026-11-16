import json
import unittest
from fixture_cleanup import finish_fixture, recover_namespace_uid


class FixtureCleanupTest(unittest.TestCase):
    def test_report_storage_failure_cannot_skip_cleanup(self):
        report = {'status': 'passed', 'cleanupComplete': False}
        events = []
        def persist():
            events.append('write')
            raise OSError('synthetic full disk')
        def cleanup(checkpoint):
            events.append('cleanup')
            checkpoint()
            events.append('namespace-deleted')
            report['cleanupComplete'] = True
        with self.assertRaises(OSError):
            finish_fixture(report, persist, cleanup)
        self.assertIn('namespace-deleted', events)
        self.assertTrue(report['cleanupComplete'])
        self.assertEqual(report['status'], 'failed')

    def test_api_cleanup_failure_is_durably_reported(self):
        report = {'status': 'passed', 'cleanupComplete': False}
        saved = []
        def cleanup(checkpoint):
            raise TimeoutError('synthetic API wait failure')
        with self.assertRaises(TimeoutError):
            finish_fixture(report, lambda: saved.append(dict(report)), cleanup)
        self.assertEqual(saved[-1]['cleanupFailure'], 'TimeoutError')
        self.assertFalse(saved[-1]['cleanupComplete'])
        self.assertEqual(saved[-1]['status'], 'failed')

    def test_lost_create_response_recovers_only_nonce_bound_cleanup_identity(self):
        uid = '11111111-1111-1111-1111-111111111111'
        obj = {'metadata': {'name': 'ai-sdlc-builds', 'uid': uid, 'labels': {'fixture': 'owned-nonce'}}}
        call = lambda argv: json.dumps(obj)
        self.assertEqual(recover_namespace_uid(call, 'ai-sdlc-builds', 'fixture', 'owned-nonce'), uid)
        with self.assertRaises(ValueError):
            recover_namespace_uid(call, 'ai-sdlc-builds', 'fixture', 'foreign-nonce')
        self.assertIsNone(recover_namespace_uid(lambda argv: '', 'ai-sdlc-builds', 'fixture', 'owned-nonce'))


if __name__ == '__main__':
    unittest.main()
