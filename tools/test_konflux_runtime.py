import copy
import unittest
import subprocess
import tempfile
from pathlib import Path

import konflux_build_fixture as fixture
from verify_konflux_runtime import verified_image, runtime_identity, cleanup_runtime_namespace


class RuntimeInputTests(unittest.TestCase):
    def test_unverified_or_different_artifact_never_reaches_runtime_setup(self):
        good = {'image': 'registry-service.kind-registry/ai-sdlc-fixtures/homedir@sha256:'+'a'*64,
                'sourceSHA': fixture.INPUTS['sourceSHA'],
                'status': 'isolated-signatures-and-identity-verified',
                'imageSignatureVerified': True, 'attestationSignatureVerified': True,
                'signedIdentityBound': True, 'wrongKeyRejected': True, 'missingArtifactRejected': True}
        self.assertEqual(verified_image(good), good['image'])
        for key, value in [('image', 'public.example/untrusted:latest'), ('image', good['image']+'?other'),
                           ('sourceSHA', 'b'*40), ('status', 'running'), ('imageSignatureVerified', False),
                           ('attestationSignatureVerified', True+0), ('signedIdentityBound', False),
                           ('wrongKeyRejected', False), ('missingArtifactRejected', False)]:
            with self.subTest(key=key, value=value):
                changed = copy.deepcopy(good); changed[key] = value
                with self.assertRaises(ValueError): verified_image(changed)


class RuntimeIdentityTests(unittest.TestCase):
    def test_original_container_required_and_credentials_rejected(self):
        pod = {'metadata': {'uid': 'pod'}, 'spec': {'containers': [{'name': 'homedir', 'image': 'image'}],
               'automountServiceAccountToken': False, 'volumes': []},
               'status': {'containerStatuses': [{'name': 'homedir', 'imageID': 'image', 'containerID': 'original', 'restartCount': 0, 'state': {'running': {}}}]}}
        self.assertEqual(runtime_identity(pod, 'pod', 'image'), 'original')
        for case in ('uid', 'image', 'restart', 'process', 'dead', 'token', 'secret', 'ephemeral'):
            with self.subTest(case=case):
                p = copy.deepcopy(pod)
                if case == 'uid': p['metadata']['uid'] = 'replacement'
                if case == 'image': p['spec']['containers'][0]['image'] = 'other'
                if case == 'restart': p['status']['containerStatuses'][0]['restartCount'] = 1
                if case == 'process': p['status']['containerStatuses'][0]['containerID'] = 'new'
                if case == 'dead': p['status']['containerStatuses'][0]['state'] = {'terminated': {'exitCode': 0}}
                if case == 'token': p['spec']['automountServiceAccountToken'] = True
                if case == 'secret': p['spec']['volumes'] = [{'secret': {'secretName': 'unexpected'}}]
                if case == 'ephemeral': p['spec']['ephemeralContainers'] = [{'name': 'debug'}]
                with self.assertRaises(ValueError): runtime_identity(p, 'pod', 'image', 'original')

    def test_log_timeout_still_attempts_uid_cleanup_and_api_failure_is_recorded(self):
        for delete_fails in (False, True):
            with self.subTest(delete_fails=delete_fails), tempfile.TemporaryDirectory() as directory:
                calls = []; report = {}; saved = []
                def call(args, value=None, **kwargs):
                    calls.append((args, value))
                    if args[0] == 'logs':
                        self.assertIn('--limit-bytes=32768', args)
                        raise subprocess.TimeoutExpired(args, 10)
                    if args[0] == 'delete' and delete_fails: raise ValueError('API unavailable')
                    return ''
                def run(): cleanup_runtime_namespace(call, 'fixture', 'original-uid', Path(directory), report, lambda: saved.append(dict(report)))
                if delete_fails:
                    with self.assertRaisesRegex(ValueError, 'API unavailable'): run()
                else: run()
                self.assertTrue(report['logCollectionFailed'])
                self.assertEqual(report['cleanupComplete'], not delete_fails)
                self.assertEqual(calls[1][1]['preconditions'], {'uid': 'original-uid'})
                if delete_fails: self.assertIn('cleanupFailure', report)
                self.assertFalse(saved[0]['cleanupComplete'])


if __name__ == '__main__':
    unittest.main()
