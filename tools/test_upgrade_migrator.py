"""Migration ambiguity must never cause a replay or remove a foreign container."""
import copy
import json
import unittest
import sys
import time
from unittest.mock import Mock, patch

import install
import upgrade_migrator as migration


class MigratorTests(unittest.TestCase):
    def helper(self, record=None):
        passwords = {role: str(index) * 48 for index, role in enumerate(install.ALL_ROLES)}
        return migration.Migrator(
            'podman', 'quay.io/os-santiago/ai-sdlc@sha256:' + 'a' * 64,
            'postgresql://ai_sdlc:private@127.0.0.1:54321/ai_sdlc?sslmode=disable',
            54321, passwords, {} if record is None else record, Mock())

    def identity(self, item, state='created'):
        return {'Id': item.cid, 'Config': {'Labels': {migration.LABEL: item.owner},
                'Cmd': ['migrate'], 'Image': item.image},
                'HostConfig': {'NetworkMode': 'host'},
                'State': {'Status': state, 'Running': False, 'ExitCode': 0}}

    def test_all_persisted_intents_reject_replay_before_provider_calls(self):
        for phase in (None, 'creating', 'created', 'starting', 'completed'):
            with self.subTest(phase=phase), patch.object(migration, 'capture') as execute:
                item = self.helper({'state': phase})
                with self.assertRaises(ValueError):
                    item.run()
                execute.assert_not_called()
                item.save.assert_not_called()

    def test_missing_or_foreign_custody_never_removes_or_starts_container(self):
        for fault in ('missing', 'id', 'owner', 'network', 'command', 'image'):
            with self.subTest(fault=fault):
                item = self.helper()
                item.cid = 'b' * 64 if fault != 'missing' else None
                obj = self.identity(item)
                if fault == 'id': obj['Id'] = 'c' * 64
                if fault == 'owner': obj['Config']['Labels'][migration.LABEL] = 'foreign'
                if fault == 'network': obj['HostConfig']['NetworkMode'] = 'bridge'
                if fault == 'command': obj['Config']['Cmd'] = ['other']
                if fault == 'image': obj['Config']['Image'] = 'foreign'
                with patch.object(migration, 'capture', return_value=json.dumps([obj]).encode()) as execute:
                    with self.assertRaises(ValueError): item.close()
                    self.assertFalse(any('rm' in call.args[0] or 'start' in call.args[0]
                                         for call in execute.call_args_list))
                self.assertIsNone(item.material)
                self.assertIsNone(item.connection)

    def test_ambiguous_create_preserves_intent_and_never_starts_or_adopts_by_name(self):
        item = self.helper()
        with patch.object(migration, 'capture', return_value=b'unknown acknowledgement') as execute:
            with self.assertRaises(ValueError): item.run()
            self.assertEqual(item.record['state'], 'creating')
            with self.assertRaises(ValueError): item.close()
            self.assertEqual(execute.call_count, 1)
            self.assertIn('create', execute.call_args.args[0])
        self.assertEqual(item.record['cleanup'], 'uncertain-no-original-ID')

    def test_persistence_failure_after_create_never_starts_sql(self):
        item = self.helper()
        snapshots = []
        def save(record):
            snapshots.append(copy.deepcopy(record))
            if record['state'] == 'created': raise OSError('storage unavailable')
        item.save = save
        with patch.object(migration, 'capture', return_value=('b' * 64).encode()) as execute:
            with self.assertRaises(OSError): item.run()
            self.assertEqual(execute.call_count, 1)
        self.assertEqual(item.cid, 'b' * 64)
        self.assertEqual([v['state'] for v in snapshots], ['creating', 'created'])

    def test_migrator_start_failure_is_not_reported_as_complete_or_replayed(self):
        item = self.helper()
        calls = []
        def execute(argv, **kwargs):
            calls.append((argv, kwargs))
            if 'create' in argv: return ('b' * 64).encode()
            if 'inspect' in argv: return json.dumps([self.identity(item)]).encode()
            if 'start' in argv: raise ValueError('original command outcome unavailable')
            self.fail('Unexpected provider operation')
        with patch.object(migration, 'capture', side_effect=execute):
            with self.assertRaises(ValueError): item.run()
            self.assertEqual(item.record['state'], 'starting')
            with self.assertRaises(ValueError): item.run()
        self.assertEqual(sum('start' in argv for argv, _ in calls), 1)
        create = next(call for call in calls if 'create' in call[0])
        start = next(call for call in calls if 'start' in call[0])
        self.assertNotIn(item.connection, ' '.join(create[0]))
        self.assertEqual(create[1]['env']['DATABASE_URL'], item.connection)
        self.assertEqual(start[1]['input'], item.material)
        self.assertNotIn(item.material.decode(), ' '.join(start[0]))

    def test_expiration_during_custody_check_prevents_create_or_start(self):
        for expire_on in (1, 2, 3):
            with self.subTest(expire_on=expire_on):
                item = self.helper()
                observed = []
                calls = []
                def guard():
                    observed.append(True)
                    if len(observed) == expire_on: item.expires_at = time.time() - 1
                def execute(argv, **kwargs):
                    calls.append(argv)
                    if 'create' in argv: return ('b' * 64).encode()
                    if 'inspect' in argv: return json.dumps([self.identity(item)]).encode()
                    if 'rm' in argv or 'ps' in argv: return b''
                    self.fail('Migration started after expiry')
                item.guard = guard
                with patch.object(migration, 'capture', side_effect=execute):
                    with self.assertRaises(ValueError): item.run()
                    self.assertFalse(any('start' in argv for argv in calls))
                    if expire_on < 3:
                        self.assertFalse(any('create' in argv for argv in calls))
                    else:
                        item.close()  # Original CID cleanup remains available after expiry.
                        self.assertEqual(item.record['cleanup'], 'removed')

    def test_real_process_cannot_outlive_original_remaining_migration_window(self):
        item = self.helper()
        item.expires_at = time.time() + .1
        started = time.monotonic()
        with self.assertRaises(ValueError):
            item.command([sys.executable, '-c', 'import time;time.sleep(60)'], timeout=120)
        self.assertLess(time.monotonic() - started, 2)


if __name__ == '__main__':
    unittest.main()
