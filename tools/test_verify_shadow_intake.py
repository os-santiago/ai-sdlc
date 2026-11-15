import unittest

from verify_shadow_intake import observation_sql, verified


class ShadowObservationTests(unittest.TestCase):
    def test_incomplete_or_executing_path_cannot_pass(self):
        complete = dict(generation=2, admissionGeneration=2, planningGeneration=2,
                        admission='accepted', plans=1, blockedItems=1,
                        signedDeliveries=2, signedReceipts=2, executionRequests=0,
                        attempts=0, releaseGates=0)
        self.assertTrue(verified(complete, 2, 'accepted'))
        for key, value in [('planningGeneration', 1), ('signedReceipts', 1),
                           ('signedDeliveries', 1), ('attempts', 1), ('executionRequests', 1),
                           ('releaseGates', 1), ('blockedItems', 0)]:
            self.assertFalse(verified(dict(complete, **{key: value}), 2, 'accepted'), key)
        self.assertFalse(verified(complete, 3, 'accepted'))
        withdrawn = dict(complete, admission='rejected', plans=0, blockedItems=0)
        self.assertTrue(verified(withdrawn, 2, 'rejected'))

    def test_scope_is_validated_before_sql_construction(self):
        for repository, issue in [("owner/repo'; DELETE FROM plans;--", 1), ('../repo', 1), ('owner/repo', -1)]:
            with self.assertRaises(ValueError):
                observation_sql(repository, issue)
        self.assertIn('BEGIN READ ONLY', observation_sql('owner/repo', 42))


if __name__ == '__main__':
    unittest.main()
