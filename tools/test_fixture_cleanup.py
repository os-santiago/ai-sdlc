import json
import unittest
from fixture_cleanup import finish_fixture, recover_namespace_uid, create_global_resource, recover_global_resources, create_slot_resource, recover_slot_resources


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

    def test_lost_global_ack_recovers_only_original_resource(self):
        for resource, kind in [('persistentvolumes', 'PersistentVolume'), ('validatingadmissionpolicies', 'ValidatingAdmissionPolicy')]:
            with self.subTest(resource=resource):
                intents, created, snapshots = [], [], []
                value = {'apiVersion': 'v1' if resource == 'persistentvolumes' else 'admissionregistration.k8s.io/v1',
                         'kind': kind, 'metadata': {'name': 'fixture-resource'}}
                def persist():
                    snapshots.append(json.loads(json.dumps(intents)))
                def create(obj):
                    self.assertIsNone(snapshots[-1][0]['uid'])
                    obj['metadata']['uid'] = '11111111-1111-1111-1111-111111111111'
                    created.append(obj)
                    raise TimeoutError('real effect, lost reply')
                with self.assertRaises(TimeoutError):
                    create_global_resource(create, persist, intents, resource, value, 'namespace-uid', 'nonce')
                recover_global_resources(lambda argv: json.dumps(created[0]), intents)
                self.assertEqual(intents[0]['uid'], created[0]['metadata']['uid'])
                created[0]['metadata']['uid'] = '22222222-2222-2222-2222-222222222222'
                with self.assertRaises(ValueError):
                    recover_global_resources(lambda argv: json.dumps(created[0]), intents)
                created[0]['metadata']['labels']['ai-sdlc.closed-build-fixture'] = 'foreign'
                intents[0]['uid'] = None
                with self.assertRaises(ValueError):
                    recover_global_resources(lambda argv: json.dumps(created[0]), intents)

    def test_slot_resource_intents_survive_lost_ack_and_reject_replacement(self):
        for kind in ('Namespace', 'PersistentVolume', 'PersistentVolumeClaim'):
            with self.subTest(kind=kind):
                intents, created, snapshots = [], [], []
                value = {'apiVersion': 'v1', 'kind': kind, 'metadata': {'name': 'fixture'}}
                if kind == 'PersistentVolumeClaim': value['metadata']['namespace'] = 'ai-sdlc-builds'
                def persist(): snapshots.append(json.loads(json.dumps(intents)))
                def create(obj):
                    self.assertIsNone(snapshots[-1][0]['uid'])
                    obj['metadata']['uid'] = '11111111-1111-1111-1111-111111111111'
                    created.append(obj)
                    raise TimeoutError('created, response lost')
                with self.assertRaises(TimeoutError):
                    create_slot_resource(create, persist, intents, value, 'cluster-uid', 'nonce')
                recover_slot_resources(lambda argv: json.dumps(created[0]), intents)
                self.assertEqual(intents[0]['uid'], created[0]['metadata']['uid'])
                original = json.loads(json.dumps(created[0]))
                for field in ('uid', 'kind', 'namespace', 'nonce', 'cluster'):
                    obj = json.loads(json.dumps(original))
                    if field == 'uid': obj['metadata']['uid'] = '22222222-2222-2222-2222-222222222222'
                    if field == 'kind': obj['kind'] = 'Secret'
                    if field == 'namespace': obj['metadata']['namespace'] = 'foreign'
                    if field == 'nonce': obj['metadata']['labels']['ai-sdlc.slot-api-fixture'] = 'foreign'
                    if field == 'cluster': obj['metadata']['annotations']['ai-sdlc.fixture-cluster-uid'] = 'foreign'
                    with self.subTest(field=field), self.assertRaises(ValueError):
                        recover_slot_resources(lambda argv: json.dumps(obj), intents)
                recover_slot_resources(lambda argv: '', intents)
                self.assertTrue(intents[0]['absent'])


if __name__ == '__main__':
    unittest.main()
