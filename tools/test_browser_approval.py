"""Finite browser approval custody and unchanged runtime authority."""
import copy
import json
from pathlib import Path
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import bootstrap_prepare as bootstrap
import browser_approval_state as history
import browser_approval_window as renewal
import approval_window_targets
import test_review_feedback_upgrade


def operation(prefix=(), number=1):
    approvals = {'provenance': {'expiresAt': number}, 'release': {'expiresAt': number}}
    return {'version': history.OPERATION, 'mode': 'browser-approval',
        'nonce': format(number, '032x'), 'parentSHA256': history.source_digest('browser', list(prefix)),
        'sourceApprovalCount': len(prefix), 'completed': 'source', 'accepted': False,
        'approvals': approvals, 'approvalsSHA256': bootstrap.digest(approvals),
        'createdAt': 1, 'expiresAt': 2, 'installationReports': []}


def accepted(record):
    result = copy.deepcopy(record)
    result.update(completed='active', accepted=True,
        backup={'phase': 'verified', 'restore': {'state': 'removed'}},
        installationReports=[{'configuration': 'active', 'installationReady': True}])
    return result


class ApprovalHistory(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.journal = history.Journal(self.directory, 'browser')

    def test_two_operations_preserve_parent_and_reject_historical_reuse(self):
        first = operation()
        self.journal.save(0, first)
        first = accepted(first)
        self.journal.save(0, first)
        second = operation([first], 2)
        self.journal.save(1, second)
        second = accepted(second)
        self.journal.save(1, second)
        loaded = history.Journal(self.directory, 'browser')
        self.assertEqual(loaded.value['operations'], [first, second])
        self.assertEqual(loaded.select(second['approvals']), 1)
        with self.assertRaises(ValueError):
            loaded.select(first['approvals'])
        for index, record in ((0, first), (1, dict(second, expiresAt=3))):
            with self.assertRaises(ValueError):
                loaded.save(index, record)

    def test_pending_resume_has_same_nonce_and_deadline(self):
        first = operation()
        self.journal.save(0, first)
        resumed = history.Journal(self.directory, 'browser')
        self.assertEqual(resumed.select(first['approvals']), 0)
        with self.assertRaises(ValueError):
            resumed.select(operation(number=2)['approvals'])
        for field, value in (('expiresAt', 999), ('nonce', 'f'*32), ('sourceProof', {'changed': True})):
            with self.subTest(field=field), self.assertRaises(ValueError):
                resumed.save(0, dict(first, **{field: value}))
        self.assertEqual(resumed.read()['operations'], [first])

    def test_competing_writer_is_rejected_before_write(self):
        stale = history.Journal(self.directory, 'browser')
        self.journal.save(0, operation())
        with self.assertRaises(ValueError):
            stale.save(0, operation(number=2))
        self.assertEqual(self.journal.read(), self.journal.value)

    def test_unknown_write_ack_resumes_exact_persisted_operation(self):
        write = history.platform.write_record
        def lost_ack(path, value):
            write(path, value)
            raise TimeoutError('lost acknowledgement')
        first = operation()
        with patch.object(history.platform, 'write_record', side_effect=lost_ack), self.assertRaises(TimeoutError):
            self.journal.save(0, first)
        resumed = history.Journal(self.directory, 'browser')
        self.assertEqual(resumed.select(first['approvals']), 0)
        self.assertEqual(resumed.value['operations'], [first])
        with self.assertRaises(ValueError):
            self.journal.save(0, first)

    def test_completed_record_requires_backup_cleanup_and_ready_report(self):
        good = accepted(operation())
        for key, value in (('completed', 'armed'), ('backup', {'phase': 'verified', 'restore': {'state': 'unknown'}}),
                           ('transition', {'to': 'active'}), ('installationReports', [])):
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.journal.save(0, dict(good, **{key: value}))
        self.assertFalse(self.journal.path.exists())

    def test_bound_and_parent_digest_prevent_unbounded_or_forked_history(self):
        for index in range(history.MAX_OPERATIONS):
            op = accepted(operation(self.journal.value['operations'], index+1))
            self.journal.save(index, op)
        with self.assertRaises(ValueError):
            self.journal.select(operation(number=100)['approvals'])
        bad = copy.deepcopy(self.journal.value)
        bad['operations'][-1]['parentSHA256'] = 'another-parent'
        with self.assertRaises(ValueError):
            history.validate(bad, 'browser')


class BrowserRenewal(unittest.TestCase):
    def test_actual_ten_consumer_chart_preserves_images_profiles_and_intake_last(self):
        source, render, compiler, _, _ = test_review_feedback_upgrade.FeedbackTargets().source()
        now = time.time()
        approvals = {}
        for section, key in (('provenance', 'trustJSON'), ('release', 'targetJSON')):
            original = json.loads(source['values'][section][key])
            original.update(approvedAt=int((now-3600)*1000), expiresAt=int((now-1)*1000))
            source['values'][section][key] = json.dumps(original, sort_keys=True)
            approvals[section] = dict(original, approvedAt=int((now-10)*1000), expiresAt=int((now+3600)*1000))
        source['documents'] = render(source['values'])
        before = copy.deepcopy(source)
        result = approval_window_targets.targets(source, approvals, render, compiler, now)
        self.assertEqual(source, before)
        for phase, target in result.items():
            self.assertEqual(target['images'], source['images'])
            self.assertEqual(target['values']['release']['browserProfileJSON'], source['values']['release']['browserProfileJSON'])
            deployments = [obj for obj in target['documents'] if obj['kind'] == 'Deployment']
            self.assertEqual(len(deployments), 10)
            intake = next(obj for obj in deployments if obj['metadata']['name'] == 'ai-sdlc-intake')
            self.assertEqual(intake['spec']['replicas'], int(phase in ('source', 'active')))
        restored = copy.deepcopy(result['active']['values'])
        for section, key in (('provenance', 'trustJSON'), ('release', 'targetJSON')):
            restored[section][key] = source['values'][section][key]
        self.assertEqual(render(restored), source['documents'])

    def test_expired_pending_intent_cannot_resume_mutations(self):
        item = object.__new__(renewal.BrowserApproval)
        item.record = {'accepted': False, 'expiresAt': time.time()-1}
        item.save, item.pause_current, item.apply = Mock(), Mock(), Mock()
        with self.assertRaises(ValueError):
            item.run()
        item.save.assert_not_called()
        item.pause_current.assert_not_called()
        item.apply.assert_not_called()

    def test_missing_documents_rejected_before_observation_or_effect(self):
        item = object.__new__(renewal.BrowserApproval)
        item.record, item.approvals = None, None
        item.observe, item.save = Mock(), Mock()
        with self.assertRaises(ValueError):
            item.begin()
        item.observe.assert_not_called()
        item.save.assert_not_called()

    def test_residue_rechecked_under_fence_before_capture(self):
        item = object.__new__(renewal.BrowserApproval)
        item.observe = Mock(side_effect=ValueError('browser job remains'))
        with patch.object(renewal.ReviewFeedbackUpgrade, 'capture_first_pause') as capture, self.assertRaises(ValueError):
            item.capture_first_pause(Mock())
        capture.assert_not_called()


class CurrentApprovalSource(unittest.TestCase):
    def test_feedback_source_binds_exact_completed_prefix_and_rejects_new_approval(self):
        import browser_runtime_upgrade
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory)
            original = {'completed': 'active', 'mode': 'browser-evidence',
                'migrationVerified': True, 'installationReports': [{'original': True}],
                'backup': {'phase': 'verified', 'restore': {'state': 'removed'}}}
            original_hash = bootstrap.digest(original)
            path = state / 'browser-runtime-upgrade.json'
            path.write_text(json.dumps(original))
            browser = SimpleNamespace(record=original, path=path, parent=Mock(), pause=Mock(), helper='original',
                describe=Mock(return_value={'original': 'render'}), custody=Mock(), close=Mock())
            with patch.object(browser_runtime_upgrade, 'BrowserUpgrade', return_value=browser):
                source = history.CurrentSource(SimpleNamespace(state_dir=state))
                self.assertEqual(source.digest(), original_hash)
                self.assertEqual(source.describe('active'), {'original': 'render'})
                journal = history.Journal(state, original_hash)
                pending = operation()
                pending['parentSHA256'] = original_hash
                pending['targets'] = {'active': {'renewed': 'render'}}
                journal.save(0, pending)
                with self.assertRaises(ValueError):
                    source.custody()
                with self.assertRaises(ValueError):
                    history.CurrentSource(SimpleNamespace(state_dir=state))
                own_pending = history.CurrentSource(SimpleNamespace(state_dir=state), 0, pending['nonce'])
                own_pending.custody()
                complete = accepted(pending)
                complete['targets'] = {'active': {'renewed': 'render'}}
                journal.save(0, complete)
                installed = history.CurrentSource(SimpleNamespace(state_dir=state))
                self.assertEqual(installed.describe('active'), {'renewed': 'render'})
                self.assertEqual(installed.current_report, complete['installationReports'][-1])
                self.assertNotEqual(installed.digest(), original_hash)
                original['unexpected'] = True
                path.write_text(json.dumps(original))
                with self.assertRaises(ValueError):
                    installed.custody()

    def test_source_requires_verified_backup_and_removed_restore_before_any_effect(self):
        import browser_runtime_upgrade
        original = {'completed': 'active', 'mode': 'browser-evidence', 'migrationVerified': True}
        for backup in (None, {}, {'phase': 'capturing'}, {'phase': 'verified', 'restore': {'state': 'running'}}):
            browser = SimpleNamespace(record=dict(original, backup=backup), close=Mock())
            with self.subTest(backup=backup), patch.object(browser_runtime_upgrade, 'BrowserUpgrade', return_value=browser), \
                    patch.object(history, 'Journal') as journal, self.assertRaises(ValueError):
                history.CurrentSource(SimpleNamespace())
            journal.assert_not_called()
            browser.close.assert_called_once()

    def test_completed_report_lost_ack_reconciles_without_new_operation_or_timestamp(self):
        with tempfile.TemporaryDirectory() as directory:
            item = object.__new__(renewal.BrowserApproval)
            item.config = SimpleNamespace(state_dir=Path(directory))
            item.parent_hash = 'original'
            item.record = {'nonce': 'a'*32, 'accepted': False, 'installationSource': {},
                'approvalsSHA256': 'reviewed', 'installationReports': []}
            item.observe, item.save = Mock(), Mock()
            write = renewal.platform.write_record
            def lost_ack(path, value):
                write(path, value)
                raise TimeoutError('lost report acknowledgement')
            with patch.object(renewal.platform, 'write_record', side_effect=lost_ack), self.assertRaises(TimeoutError):
                item.publish_report(active=True)
            persisted = copy.deepcopy(item.record)
            item.save.reset_mock()
            item.publish_report(active=True)
            self.assertEqual(item.record, persisted)
            item.save.assert_not_called()
            report = json.loads((Path(directory)/'installation.json').read_text())
            self.assertEqual(report, persisted['installationReports'][-1])


if __name__ == '__main__':
    unittest.main()
