import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import github_merge_install as native


class NativeMergeInstallTests(unittest.TestCase):
    def fixture(self):
        return dict(copy.deepcopy(native.BODY), id=999, source=native.floor.REPOSITORY, source_type='Repository')

    def fake(self, records, calls, lose=False, duplicate=False):
        def request(method, path, body=None):
            calls.append((method, path))
            if method == 'POST':
                self.assertEqual(body, native.BODY)
                records.append(self.fixture())
                if lose:
                    raise ValueError('lost response')
                return records[-1]
            if path == native.BASE + '?per_page=100&includes_parents=true':
                return copy.deepcopy(records + ([self.fixture()] if duplicate and records else []))
            return next(copy.deepcopy(r) for r in records if str(r['id']) == path.rsplit('/', 1)[1])
        return request

    def test_additive_create_reapply_preserves_existing_and_pins_id(self):
        old = {'id': 9071701, 'name': 'Main Branch Protection', 'strict': False, 'bypass': True}
        records, calls = [copy.deepcopy(old)], []
        with tempfile.TemporaryDirectory() as tmp, patch.object(native, 'request', side_effect=self.fake(records, calls)):
            first = native.reconcile(tmp)
            second = native.reconcile(tmp)
            self.assertEqual(first, second)
            self.assertEqual(first['RulesetID'], 999)
            self.assertEqual(records[0], old)
            self.assertEqual([m for m, _ in calls if m != 'GET'], ['POST'])
            receipt = json.loads((Path(tmp) / 'merge-protection.json').read_text())
            self.assertEqual(receipt['state'], 'ready')
            self.assertEqual(receipt['id'], 999)

    def test_lost_create_response_is_not_retried_or_adopted(self):
        records, calls = [], []
        with tempfile.TemporaryDirectory() as tmp, patch.object(native, 'request', side_effect=self.fake(records, calls, lose=True)):
            for _ in range(2):
                with self.assertRaises(ValueError):
                    native.reconcile(tmp)
            self.assertEqual([m for m, _ in calls if m != 'GET'], ['POST'])
            self.assertEqual(len(records), 1)

    def test_scope_drift_bypass_and_ambiguous_identity_deny(self):
        for field, value in [('source', 'other/repo'), ('name', 'other'), ('enforcement', 'disabled'),
                             ('id', True), ('bypass_actors', [{}]), ('conditions', {})]:
            with self.assertRaises(ValueError):
                native.validate(dict(self.fixture(), **{field: value}))
        for field, value in [('strict_required_status_checks_policy', False), ('required_status_checks', [])]:
            r = self.fixture(); r['rules'][1]['parameters'][field] = value
            with self.assertRaises(ValueError):
                native.validate(r)
        records, calls = [self.fixture(), self.fixture()], []
        with tempfile.TemporaryDirectory() as tmp, patch.object(native, 'request', side_effect=self.fake(records, calls)):
            with self.assertRaises(ValueError):
                native.reconcile(tmp)
            self.assertTrue(all(m == 'GET' for m, _ in calls))

    def test_concurrent_stricter_old_policy_survives_and_duplicate_create_blocks(self):
        records, calls = [{'id': 9071701, 'name': 'Old', 'approvals': 0}], []
        fake = self.fake(records, calls)
        def request(method, path, body=None):
            if method == 'POST':
                records[0]['approvals'] = 2
            return fake(method, path, body)
        with tempfile.TemporaryDirectory() as tmp, patch.object(native, 'request', side_effect=request):
            native.reconcile(tmp)
            self.assertEqual(records[0]['approvals'], 2)
        records, calls = [], []
        with tempfile.TemporaryDirectory() as tmp, patch.object(native, 'request', side_effect=self.fake(records, calls, duplicate=True)):
            with self.assertRaises(ValueError):
                native.reconcile(tmp)
            self.assertEqual(json.loads((Path(tmp)/'merge-protection.json').read_text())['state'], 'sending')

    def test_local_lock_prevents_second_installation_and_disallows_mutation_methods(self):
        import fcntl
        with tempfile.TemporaryDirectory() as tmp:
            with (Path(tmp) / 'merge-protection.lock').open('w') as lock:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                with patch.object(native, 'request') as request:
                    with self.assertRaises(ValueError):
                        native.reconcile(tmp)
                    request.assert_not_called()
        with patch.object(native.subprocess, 'run') as run:
            for method in ('PUT', 'PATCH', 'DELETE'):
                with self.assertRaises(ValueError):
                    native.request(method, native.BASE + '/9071701')
            run.assert_not_called()

    def test_first_use_persists_directory_chain_and_receipt_before_post(self):
        import os
        records, calls, synced = [], [], []
        fsync = os.fsync
        fake = self.fake(records, calls)
        with tempfile.TemporaryDirectory() as tmp:
            state = Path(tmp) / 'new-parent' / 'new-state'
            def sync(fd):
                synced.append(Path(os.readlink('/proc/self/fd/' + str(fd))))
                fsync(fd)
            def request(method, path, body=None):
                if method == 'POST':
                    self.assertIn(state.parent, synced)
                    self.assertIn(Path(tmp), synced)
                    self.assertGreaterEqual(synced.count(state), 2)
                    self.assertEqual(json.loads((state / 'merge-protection.json').read_text())['state'], 'sending')
                return fake(method, path, body)
            with patch.object(native.os, 'fsync', side_effect=sync), patch.object(native, 'request', side_effect=request):
                native.reconcile(state)

    def test_failed_durability_or_symlink_receipt_cannot_send(self):
        with tempfile.TemporaryDirectory() as tmp:
            with patch.object(native.os, 'fsync', side_effect=OSError('unavailable')), patch.object(native, 'request') as request:
                with self.assertRaises(OSError):
                    native.reconcile(Path(tmp))
                request.assert_not_called()
            (Path(tmp) / 'merge-protection.json').symlink_to(Path(tmp) / 'missing')
            with patch.object(native, 'request') as request:
                with self.assertRaises(OSError):
                    native.reconcile(Path(tmp))
                request.assert_not_called()


if __name__ == '__main__':
    unittest.main()
