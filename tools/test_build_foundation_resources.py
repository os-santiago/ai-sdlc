import copy
import unittest
from build_foundation_state import ANNOTATION
from verify_build_foundation_resources import cleanup_uid


class FoundationResourceCleanupTests(unittest.TestCase):
    def test_cleanup_recovers_only_original_nonce_and_uid(self):
        uid = '11111111-1111-4111-8111-111111111111'
        entry = {'nonce': 'original', 'uid': None}
        obj = {'metadata': {'uid': uid, 'annotations': {ANNOTATION: 'original'}}}
        self.assertEqual(cleanup_uid(obj, entry), uid)
        self.assertEqual(cleanup_uid(obj, dict(entry, uid=uid)), uid)
        for bad in ({'nonce': 'other', 'uid': None}, {'nonce': 'original', 'uid': '22222222-2222-4222-8222-222222222222'}):
            with self.assertRaises(ValueError):
                cleanup_uid(obj, bad)
        changed = copy.deepcopy(obj)
        changed['metadata']['uid'] = 'not-a-uid'
        with self.assertRaises(ValueError):
            cleanup_uid(changed, entry)


if __name__ == '__main__':
    unittest.main()
