import unittest
from closed_build_journal_fixture import validate_execution

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
