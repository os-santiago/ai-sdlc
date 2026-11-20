"""Transition acknowledgements, history and rollback authority remain bounded."""
import copy
import time
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import diagnostic_runtime_upgrade as upgrade


class TransitionTests(unittest.TestCase):
    def test_lost_helm_ack_never_replays_even_when_source_still_observed(self):
        record = {'completed': 'migrated', 'transition': None, 'expiresAt': time.time() + 60}
        snapshots = []
        observe = Mock()
        apply = Mock(side_effect=OSError('ack unavailable'))
        save = lambda value: snapshots.append(copy.deepcopy(value))
        with self.assertRaises(OSError):
            upgrade.advance(record, 'staged', observe, apply, save)
        # Recorded as sent before Helm is invoked: a lost acknowledgement never replays.
        self.assertEqual(snapshots[-1]['transition'], {'from': 'migrated', 'to': 'staged', 'sent': True})
        observe.side_effect = ValueError('target incomplete')
        with self.assertRaises(ValueError):
            upgrade.advance(record, 'staged', observe, apply, save)
        self.assertEqual(apply.call_count, 1)

    def test_lost_journal_ack_can_finish_by_observation_without_helm(self):
        record = {'completed': 'migrated', 'transition': {'from': 'migrated', 'to': 'staged'},
                  'expiresAt': time.time() + 60}
        observe, apply, save = Mock(), Mock(), Mock()
        upgrade.advance(record, 'staged', observe, apply, save)
        observe.assert_called_once_with('staged')
        apply.assert_not_called()
        self.assertEqual(record['completed'], 'staged')
        self.assertIsNone(record['transition'])

    def test_expiry_wrong_order_and_original_drift_prevent_effect(self):
        for fault in ('expiry', 'order', 'identity'):
            with self.subTest(fault=fault):
                record = {'completed': 'source' if fault == 'order' else 'migrated', 'transition': None,
                          'expiresAt': time.time() + (-1 if fault == 'expiry' else 60)}
                observe, apply, save = Mock(), Mock(), Mock()
                if fault == 'identity': observe.side_effect = ValueError('original UID changed')
                with self.assertRaises(ValueError): upgrade.advance(record, 'staged', observe, apply, save)
                apply.assert_not_called()
                save.assert_not_called()

    def test_uncertain_migration_cannot_start_another_helper(self):
        for state in ('creating', 'created', 'starting', 'completed'):
            with self.subTest(state=state):
                item = object.__new__(upgrade.Upgrade)
                item.record = {'migration': {'state': state}, 'completed': 'source'}
                item.deadline = Mock()
                item.observe = Mock(return_value=({}, 'private'))
                with patch.object(upgrade, 'Migrator') as helper:
                    with self.assertRaises(ValueError): item.migrate()
                    helper.assert_not_called()

    def test_recovery_cannot_ignore_an_uncertain_helm_target_or_helper(self):
        for fault in ('helm', 'helper', 'prior-recovery'):
            with self.subTest(fault=fault):
                item = object.__new__(upgrade.Upgrade)
                item.record = {'completed': 'migrated', 'transition': None, 'expiresAt': time.time() + 60,
                               'migration': {'state': 'starting', 'cleanup': 'removed'}}
                if fault == 'helm': item.record['transition'] = {'from': 'migrated', 'to': 'staged'}
                if fault == 'helper': item.record['migration']['cleanup'] = 'uncertain-no-original-ID'
                if fault == 'prior-recovery': item.record['recoveryPause'] = {'phase': 'pausing'}
                item.deadline = Mock()
                item.observe = Mock(side_effect=ValueError('original outcome unavailable'))
                item.apply, item.save = Mock(), Mock()
                with patch.object(upgrade, 'Fence') as fence:
                    with self.assertRaises(ValueError): item.recover()
                    fence.assert_not_called()
                item.apply.assert_not_called()

    def test_completed_recovery_pause_retries_only_observation_and_finalization(self):
        item = object.__new__(upgrade.Upgrade)
        item.record = {'completed': 'warming', 'transition': None, 'sourceProof': {},
                       'recoveryPause': {'phase': 'paused', 'cleanup': 'complete', 'catalogSize': 34}}
        item.deadline, item.observe, item.database = Mock(), Mock(), Mock()
        item.finish_recovery = Mock(return_value={'phase': 'active'})
        with patch.object(upgrade, 'Compatibility') as compatibility, patch.object(upgrade, 'Fence') as fence:
            compatibility.return_value.verify.return_value = {'catalogSize': 34}
            item.recover()
            fence.assert_not_called()
        item.observe.assert_called_once_with('source')
        item.finish_recovery.assert_called_once_with({'catalogSize': 34})


if __name__ == '__main__':
    unittest.main()
