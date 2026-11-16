"""Exercise administrative interruption/CAS boundaries, without workflow state."""
import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from provenance_registry_state import RegistryJournal

UID = '11111111-1111-1111-1111-111111111111'
OTHER = '22222222-2222-2222-2222-222222222222'
CID = 'containerd://' + 'a' * 64
NEXT = 'containerd://' + 'b' * 64


class RegistryJournalTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name) / 'journal.json'
        self.state = {'version': 'ai-sdlc.registry-upgrade-journal/v1', 'changes': {}, 'restarts': {}}
        self.frozen = True
        self.writes = self.signals = 0
        self.before = {'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {
            'name': 'registry-config', 'namespace': 'fixture', 'uid': UID, 'resourceVersion': '1'},
            'data': {'config.json': 'original-synthetic-config'}}
        self.current = copy.deepcopy(self.before)
        self.after = {'config.json': 'restrictive-synthetic-config'}
        self.task = {'podUID': UID, 'containerID': CID, 'restartCount': 0,
                     'volumeIdentity': '1:123', 'configurationSHA256': 'c' * 64, 'ready': True, 'running': True}
        self.journal = RegistryJournal(self.path, self.state, self.fence)

    def fence(self):
        if not self.frozen:
            raise ValueError('fixture authority revoked')

    def read(self):
        return copy.deepcopy(self.current)

    def replace(self, value):
        saved = json.loads(self.path.read_text())
        self.assertEqual(next(iter(saved['changes'].values()))['phase'], 'prepared')
        if any(value['metadata'][k] != self.current['metadata'][k] for k in ('uid', 'resourceVersion')):
            raise ValueError('fixture API CAS conflict')
        self.writes += 1
        self.current = copy.deepcopy(value)
        self.current['metadata']['resourceVersion'] = str(int(value['metadata']['resourceVersion']) + 1)

    def restart(self, task_id):
        saved = json.loads(self.path.read_text())['restarts']['acl']
        self.assertGreaterEqual(saved['attempts'], 1)
        self.assertEqual(task_id, CID)
        self.assertEqual(self.task['containerID'], CID)
        self.signals += 1
        self.task.update(containerID=NEXT, restartCount=1)

    def resume(self):
        self.state = json.loads(self.path.read_text())
        self.journal = RegistryJournal(self.path, self.state, self.fence)

    def change(self, replace=None):
        return self.journal.change_data(self.before, self.after, self.read, replace or self.replace)

    def signal(self, callback=None):
        return self.journal.restart_original('acl', 'c'*64, lambda: copy.deepcopy(self.task), callback or self.restart)

    def test_lost_update_ack_recovers_exact_result_without_another_write(self):
        def lost(value):
            self.replace(value)
            raise TimeoutError('fixture lost ACK')
        with self.assertRaises(TimeoutError):
            self.change(lost)
        self.resume()
        self.assertEqual(self.change()['data'], self.after)
        self.change()
        self.assertEqual(self.writes, 1)
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)
        self.assertNotIn('restrictive-synthetic-config', self.path.read_text())

    def test_interruption_after_intent_before_effect_can_resume(self):
        save = self.journal.save
        def interrupted(candidate):
            save(candidate)
            raise TimeoutError('fixture interruption before effect')
        self.journal.save = interrupted
        with self.assertRaises(TimeoutError): self.change()
        self.assertEqual(self.writes, 0)
        self.resume(); self.change()
        self.assertEqual(self.writes, 1)

    def test_revoked_freeze_after_intent_prevents_effect_and_reconcile(self):
        save = self.journal.save
        def revoke(candidate): save(candidate); self.frozen = False
        self.journal.save = revoke
        with self.assertRaises(ValueError): self.change()
        self.assertEqual(self.writes, 0)
        self.resume()
        with self.assertRaises(ValueError): self.change()
        self.frozen = True; self.change()
        self.assertEqual(self.writes, 1)

    def test_concurrent_cas_conflict_never_overwrites_foreign_change(self):
        def conflict(value):
            self.current['metadata']['resourceVersion'] = '2'
            self.current['data'] = {'config.json': 'foreign'}
            self.replace(value)
        with self.assertRaises(ValueError): self.change(conflict)
        self.resume()
        with self.assertRaises(ValueError): self.change()
        self.assertEqual(self.current['data'], {'config.json': 'foreign'})
        self.assertEqual(self.writes, 0)

    def test_applied_transition_rejects_replacement_drift_or_rollback(self):
        self.change(); good = copy.deepcopy(self.current)
        for fault in ('uid', 'data', 'version', 'label', 'deleting'):
            with self.subTest(fault=fault):
                self.current = copy.deepcopy(good)
                if fault == 'uid': self.current['metadata']['uid'] = OTHER
                elif fault == 'data': self.current['data'] = self.before['data']
                elif fault == 'version': self.current['metadata']['resourceVersion'] = '99'
                elif fault == 'label': self.current['metadata']['labels'] = {'injected': 'true'}
                else: self.current['metadata']['deletionTimestamp'] = 'now'
                with self.assertRaises(ValueError): self.change()
                self.assertEqual(self.writes, 1)

    def test_changed_desired_state_cannot_redefine_recorded_intent(self):
        self.change(); self.after = {'config.json': 'permissive'}
        with self.assertRaises(ValueError): self.change()
        self.assertEqual(self.writes, 1)

    def test_lost_signal_ack_accepts_only_original_pod_successor_once(self):
        def lost(task): self.restart(task); raise TimeoutError('fixture lost signal ACK')
        with self.assertRaises(TimeoutError): self.signal(lost)
        self.resume()
        self.assertTrue(self.signal())
        self.assertTrue(self.signal())
        self.assertEqual(self.signals, 1)

    def test_interrupted_signal_intent_retries_only_original_live_task(self):
        def interrupted(task): raise TimeoutError('fixture no effect before interruption')
        with patch('provenance_registry_state.time.time', return_value=100):
            with self.assertRaises(TimeoutError): self.signal(interrupted)
            self.resume()
            self.assertFalse(self.signal())  # No immediate duplicate signal.
        with patch('provenance_registry_state.time.time', return_value=106):
            self.assertFalse(self.signal())
            self.assertTrue(self.signal())
        self.assertEqual(self.signals, 1)
        self.assertEqual(self.state['restarts']['acl']['attempts'], 2)

    def test_signal_ambiguity_is_bounded_and_stopping_task_is_not_signaled(self):
        calls = []
        def no_effect(task): calls.append(task)
        for moment in (100, 106):
            with patch('provenance_registry_state.time.time', return_value=moment):
                self.assertFalse(self.signal(no_effect))
        with patch('provenance_registry_state.time.time', return_value=112):
            with self.assertRaises(ValueError): self.signal(no_effect)
        self.task['running'] = False
        self.assertFalse(self.signal(no_effect))
        self.assertEqual(calls, [CID, CID])

    def test_replacement_configuration_or_extra_restarts_cannot_be_adopted(self):
        self.signal(); good = copy.deepcopy(self.task)
        for key, value in [('podUID', OTHER), ('volumeIdentity', '1:999'),
                           ('configurationSHA256', 'd'*64), ('restartCount', 2)]:
            with self.subTest(key=key):
                self.task = dict(good, **{key: value})
                with self.assertRaises(ValueError): self.signal()
                self.assertEqual(self.signals, 1)

    def test_freeze_revoked_after_signal_intent_prevents_signal(self):
        save = self.journal.save
        calls = 0
        def revoked(candidate):
            nonlocal calls
            save(candidate); calls += 1
            if calls == 2: self.frozen = False
        self.journal.save = revoked
        with self.assertRaises(ValueError): self.signal()
        self.assertEqual(self.signals, 0)

    def test_failed_persistence_cannot_publish_intent_to_same_or_shared_instance(self):
        shared = RegistryJournal(self.path, self.state, self.fence)
        with patch.object(self.journal, 'save', side_effect=OSError('fixture disk full')):
            for _ in range(2):
                with self.assertRaises(OSError): self.change()
                self.assertEqual(self.writes, 0)
                self.assertEqual(self.state['changes'], {})
        with patch.object(shared, 'save', side_effect=OSError('fixture fsync failed')):
            with self.assertRaises(OSError):
                shared.change_data(self.before, self.after, self.read, self.replace)
        self.assertFalse(self.path.exists())
        self.assertEqual(self.writes, 0)
        shared.change_data(self.before, self.after, self.read, self.replace)
        self.assertEqual(self.writes, 1)

    def test_failed_signal_persistence_never_exposes_attempt_or_successor_in_memory(self):
        with patch.object(self.journal, 'save', side_effect=OSError('fixture disk full')):
            for _ in range(2):
                with self.assertRaises(OSError): self.signal()
                self.assertEqual(self.signals, 0)
                self.assertEqual(self.state['restarts'], {})
        self.signal()
        with patch.object(self.journal, 'save', side_effect=OSError('fixture fsync failed')):
            with self.assertRaises(OSError): self.signal()
        self.assertIsNone(self.state['restarts']['acl']['successor'])
        self.assertTrue(self.signal())
        self.assertEqual(self.signals, 1)


if __name__ == '__main__':
    unittest.main()
