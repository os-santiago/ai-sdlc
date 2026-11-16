import unittest
from verify_build_slot_api import test_result

class SlotEvidenceTest(unittest.TestCase):
    def test_requires_named_pass_and_executed_counts(self):
        evidence='    fixture.go:1: AI_SDLC_SLOT_RESULT {"accepted":1,"rejected":7}\n--- PASS: TestRealBuildSlotAPICustody (0.1s)\nPASS\n'
        self.assertEqual(test_result(evidence),{'accepted':1,'rejected':7})
        for value in ('PASS\n',evidence.replace('--- PASS:','--- SKIP:'),evidence.replace('--- PASS:','--- FAIL:'),evidence.replace('TestRealBuildSlotAPICustody','OtherTest'),evidence.replace('"accepted":1','"accepted":true'),evidence+evidence):
            with self.assertRaises(ValueError):test_result(value)
