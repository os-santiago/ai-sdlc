import unittest
from closed_build_journal_fixture import validate_execution, runner_identity
from unittest.mock import patch

class JournalFixtureEvidenceTest(unittest.TestCase):
    def test_skip_or_preflight_cannot_be_reported_as_actual_execution(self):
        log='--- PASS: TestRealJournalBackedClosedBuild (90s)\nPASS\n'
        proof={'status':'passed','durableBuiltRecordValidated':True,'syntheticAuthority':True,'releaseAuthorized':False}
        validate_execution(log,proof)
        for changed in ('PASS\n',log.replace('PASS:','SKIP:'),log.replace('PASS:','FAIL:'),log.replace('TestRealJournalBackedClosedBuild','OtherTest')):
            with self.assertRaises(ValueError):validate_execution(changed,proof)
        for key in proof:
            changed=dict(proof);del changed[key]
            with self.assertRaises(ValueError):validate_execution(log,changed)

class RunnerIdentityTest(unittest.TestCase):
    def test_public_evidence_uses_host_uid_without_opening_private_inputs(self):
        with patch('closed_build_journal_fixture.os.getuid',return_value=1000), patch('closed_build_journal_fixture.os.getgid',return_value=1001):
            self.assertEqual(runner_identity('docker'),['--user','1000:1001'])
            self.assertEqual(runner_identity('podman'),['--user','1000:1001','--userns=keep-id'])
