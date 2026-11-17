"""Closed profile change and failure-preserving administrative ordering."""
import copy
import json
import time
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import candidate_install
import diagnostic_runtime_upgrade
import initial_activation
import install
import tool_budget_targets
import tool_budget_upgrade
import tool_budget_capture
import test_diagnostic_upgrade
import test_initial_activation


class TargetTests(unittest.TestCase):
    def source(self):
        source, _, render = test_diagnostic_upgrade.DiagnosticTests().source()
        lock_path = install.LOCAL / 'images.lock.json'
        candidate = candidate_install.desired_values(install.ROOT, lock_path, install.image_lock(lock_path))['candidate']
        config = json.loads(candidate['configJSON'])
        config['profile']['maxSteps'] = 16
        source['values']['candidate']['configJSON'] = json.dumps(config, sort_keys=True)
        source['values'] = initial_activation.phase_values(source['values'], 'active', test_initial_activation.compiler(source['values']))
        source['documents'] = render(source['values'])
        return source, render

    def test_actual_chart_changes_only_generator_budget_and_intake_opens_last(self):
        source, render = self.source()
        original = copy.deepcopy(source)
        targets = tool_budget_targets.targets(source, render, test_initial_activation.compiler(source['values']))
        changed = copy.deepcopy(targets['active']['values'])
        candidate = json.loads(changed['candidate']['configJSON'])
        self.assertEqual(candidate['profile']['maxSteps'], 24)
        candidate['profile']['maxSteps'] = 16
        self.assertEqual(candidate, json.loads(original['values']['candidate']['configJSON']))
        changed['candidate']['configJSON'] = original['values']['candidate']['configJSON']
        self.assertEqual(render(changed), original['documents'])
        self.assertEqual(source, original)
        for phase in ('paused', 'staged', 'warming', 'armed', 'active'):
            docs = targets[phase]['documents']
            self.assertFalse(any(d['kind'] == 'Job' for d in docs))
            deployments = [d for d in docs if d['kind'] == 'Deployment']
            self.assertEqual(len(deployments), 10)
            intake = next(d for d in deployments if d['metadata']['name'] == 'ai-sdlc-intake')
            self.assertEqual(intake['spec']['replicas'], int(phase == 'active'))
            self.assertEqual(targets[phase]['images'], original['images'])
            self.assertEqual(targets[phase]['values']['review'], original['values']['review'])
            if phase in ('paused', 'staged'):
                self.assertEqual({d['spec']['replicas'] for d in deployments}, {0})
        recovered = tool_budget_targets.targets(source, render, test_initial_activation.compiler(source['values']), original=True)
        self.assertEqual(recovered['active']['documents'], source['documents'])

    def test_unreviewed_source_limits_are_rejected(self):
        source, render = self.source()
        for field, value in [('maxSteps', 32), ('maxSeconds', 900), ('maxTokens', 8192)]:
            bad = copy.deepcopy(source)
            config = json.loads(bad['values']['candidate']['configJSON'])
            config['profile'][field] = value
            bad['values']['candidate']['configJSON'] = json.dumps(config)
            bad['documents'] = render(bad['values'])
            with self.subTest(field=field), self.assertRaises(ValueError):
                tool_budget_targets.targets(bad, render, test_initial_activation.compiler(bad['values']))


class OrderingTests(unittest.TestCase):
    def item(self):
        item = object.__new__(tool_budget_upgrade.ToolBudgetUpgrade)
        item.record = {'mode': 'tool-budget-24', 'completed': 'source', 'transition': None,
                       'pause': None, 'backup': None, 'recoveryPause': None, 'expiresAt': time.time() + 60,
                       'sourceProof': {'execution_failures': {'rows': 1, 'sha256': 'retained'}}}
        item.observe, item.save, item.apply = Mock(), Mock(), Mock()
        item.database = Mock()
        return item

    def test_changed_history_or_diagnostic_prevents_resume(self):
        item = self.item()
        item.record['completed'] = 'ready'
        item.snapshot = Mock(return_value={'execution_failures': {'rows': 0, 'sha256': 'changed'}})
        with patch.object(tool_budget_upgrade, 'OriginalReleasePreflight') as preflight:
            with self.assertRaises(ValueError): item.resume()
            preflight.assert_not_called()
        item.apply.assert_not_called()

    def test_recreated_role_identity_is_not_hidden_by_equal_backup_data(self):
        item = self.item()
        item.record['sourceRoleIdentities'] = 'original-identities'
        item.snapshot = Mock(return_value=item.record['sourceProof'])
        with patch.object(tool_budget_upgrade, 'Compatibility') as compatibility:
            compatibility.return_value.roles.return_value = 'different-identities'
            with self.assertRaises(ValueError): item.retained()

    def test_preexisting_database_authority_drift_is_not_adopted_as_new_source(self):
        fields = ('schema', 'roles', 'permissions', 'tracking')
        for changed in fields:
            item = self.item()
            item.parent = SimpleNamespace(record={'sourceProof': dict.fromkeys(fields, 'original')})
            with patch.object(tool_budget_upgrade, 'Compatibility') as compatibility:
                for field in fields:
                    getattr(compatibility.return_value, field).return_value = 'drift' if field == changed else 'original'
                with self.subTest(field=changed), self.assertRaises(ValueError): item.original_database_authority()
            item.save.assert_not_called()
            item.apply.assert_not_called()

    def test_preexisting_membership_and_role_settings_drift_prevents_effects(self):
        fields = ('schema', 'roles', 'permissions', 'tracking')
        original = {**dict.fromkeys(fields, 'original'),
                    'data': {'@membership': 'members', '@role-settings': 'settings'}}
        for changed in ('@membership', '@role-settings'):
            item = self.item()
            item.parent = SimpleNamespace(record={'sourceProof': original})
            proof = dict(original['data'], **{changed: 'changed'})
            with patch.object(tool_budget_upgrade, 'Compatibility') as compatibility:
                for field in fields:
                    getattr(compatibility.return_value, field).return_value = 'original'
                with self.subTest(field=changed), self.assertRaises(ValueError):
                    item.original_database_authority(proof)
            item.save.assert_not_called()
            item.apply.assert_not_called()

    def test_incomplete_pause_never_replays(self):
        for state in ('planned', 'pausing', 'paused'):
            item = self.item()
            item.record['pause'] = {'phase': state}
            with patch.object(tool_budget_upgrade, 'Fence') as fence:
                with self.assertRaises(ValueError): item.pause_current()
                fence.assert_not_called()
            item.apply.assert_not_called()

    def test_history_change_before_fence_prevents_pause_intent_and_helm(self):
        item = self.item()
        item.config = Mock()
        item.helper, item.catalog = 'fixture-image', []
        item.observe.return_value = ({'resources': {'pod': 'original'}}, 'private')
        item.capture_first_pause = Mock(side_effect=ValueError('work arrived before write exclusion'))
        with patch.object(tool_budget_upgrade.bootstrap_database_forward, 'original_database') as forward, \
             patch.object(tool_budget_upgrade, 'Fence'), patch.object(tool_budget_upgrade.data, 'Database'):
            forward.return_value.__enter__.return_value = ('private-forwarded', 12345)
            with self.assertRaises(ValueError): item.pause_current()
        item.save.assert_not_called()
        item.apply.assert_not_called()
        self.assertIsNone(item.record['pause'])

    def test_polling_capture_never_changes_preliminary_proof_or_deadline(self):
        item = self.item()
        original = {'intake_cursors': {'rows': 1, 'sha256': 'old'},
                    'intake_observations': {'rows': 1, 'sha256': 'old'},
                    'work_items': {'rows': 2, 'sha256': 'same'}}
        item.record['sourceProof'] = copy.deepcopy(original)
        before = copy.deepcopy(item.record)
        current = copy.deepcopy(original)
        current['intake_cursors']['sha256'] = current['intake_observations']['sha256'] = 'new'
        item.snapshot = Mock(return_value=current)
        item.original_database_authority = Mock()
        database = Mock(); database.sql.return_value = 't\n'
        self.assertEqual(item.capture_first_pause(database), current)
        self.assertEqual(item.record, before)
        item.original_database_authority.assert_called_once_with(current, database)
        for key in ('pause', 'transition', 'backup', 'recoveryPause', 'fencedSourceProof'):
            item.record[key] = {'started': True}
            with self.subTest(key=key), self.assertRaises(ValueError): item.capture_first_pause(database)
            item.record = copy.deepcopy(before)
        for fault in ('work_items', 'rows', 'binding'):
            changed = copy.deepcopy(current)
            if fault == 'work_items': changed['work_items']['sha256'] = 'changed'
            elif fault == 'rows': changed['intake_cursors']['rows'] = 2
            else: database.sql.return_value = 'f\n'
            with self.subTest(fault=fault), self.assertRaises(ValueError):
                tool_budget_capture.validate(original, changed, database)

    def test_after_fence_polling_changes_are_rejected(self):
        item = self.item()
        item.record['fencedSourceProof'] = {'intake_observations': {'rows': 1, 'sha256': 'fixed'}}
        item.snapshot = Mock(return_value={'intake_observations': {'rows': 1, 'sha256': 'changed'}})
        with self.assertRaises(ValueError): item.retained()

    def test_incomplete_backup_never_restores_or_overwrites(self):
        item = self.item()
        item.record['backup'] = {'phase': 'restoring', 'restore': {'state': 'started'}}
        with patch.object(tool_budget_upgrade, 'Restore') as restore:
            with self.assertRaises(ValueError): item.backup()
            restore.assert_not_called()

    def test_lost_target_ack_uses_observation_without_another_send(self):
        item = self.item()
        item.record.update(completed='ready', transition={'from': 'ready', 'to': 'staged'})
        diagnostic_runtime_upgrade.advance(item.record, 'staged', item.observe, item.apply, item.save,
                                           phases=tool_budget_upgrade.PHASES)
        item.observe.assert_called_once_with('staged')
        item.apply.assert_not_called()
        self.assertEqual(item.record['completed'], 'staged')

    def test_recovery_refuses_uncertain_backup_cleanup(self):
        item = self.item()
        item.record['backup'] = {'phase': 'restoring', 'restore': {'state': 'uncertain-no-original-ID'}}
        item.pause_current = Mock()
        with self.assertRaises(ValueError): item.recover()
        item.pause_current.assert_not_called()


if __name__ == '__main__':
    unittest.main()
