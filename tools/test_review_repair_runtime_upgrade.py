"""Fail-closed guards of the schema36-to-37 repair installation operation."""
import json
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch

import review_repair_runtime_upgrade as upgrade
import runtime_upgrade_pause


def bare(**fields):
    item = object.__new__(upgrade.ReviewRepairUpgrade)
    item.save, item.apply, item.database = Mock(), Mock(), Mock()
    for key, value in fields.items():
        setattr(item, key, value)
    return item


class RepairGuards(unittest.TestCase):
    def test_begin_requires_publication_and_both_approvals_before_any_effect(self):
        for selection, approvals in ((None, {'provenance': {}, 'release': {}}), ({'images': {}}, None)):
            item = bare(record=None, selection=selection, approvals=approvals)
            item.observe = Mock()
            with self.subTest(selection=selection, approvals=approvals), \
                    patch.object(upgrade.build_runtime_install, 'component_call') as effect, \
                    self.assertRaises(ValueError):
                item.begin()
            item.observe.assert_not_called()
            effect.assert_not_called()
            item.save.assert_not_called()

    def test_residual_browser_prevents_first_journal_or_effect(self):
        uid = '11111111-1111-4111-8111-111111111111'
        item = bare(record=None, selection={'images': {}}, approvals={'provenance': {}, 'release': {}})
        observer = Mock()
        observer.get.side_effect = [{'metadata': {'uid': uid}}, {'items': [{'kind': 'Pod'}]}]
        item.observe = Mock(side_effect=lambda phase: upgrade.no_browser_execution(observer, uid))
        with patch.object(upgrade.build_runtime_install, 'component_call') as effect, self.assertRaises(ValueError):
            item.begin()
        effect.assert_not_called()
        item.save.assert_not_called()
        item.apply.assert_not_called()
        item.database.assert_not_called()

    def test_source_metadata_oracle_is_the_completed_feedback_snapshot(self):
        fields = ('@schema', '@roles', '@membership', '@role-settings', 'schema_migrations')
        original = dict.fromkeys(fields, 'original')
        item = bare(source_browser=SimpleNamespace(record={'migratedProof': original, 'sourceRoleIdentities': 'roles'}))
        for field in fields:
            with self.subTest(field=field), self.assertRaises(ValueError):
                item.original_database_authority(dict(original, **{field: 'drift'}), Mock())
        item.save.assert_not_called()
        item.apply.assert_not_called()

    def test_unknown_migration_outcome_is_not_replayed(self):
        item = bare(record={'migration': {'state': 'starting'}, 'expiresAt': 1e12})
        item.deadline = Mock()
        item.observe = Mock(return_value=({}, 'connection'))
        item.retained = Mock()
        item.config = Mock()
        with patch.object(upgrade, 'Migrator') as migrator, self.assertRaises(ValueError):
            item.migrate()
        migrator.assert_not_called()
        item.config.read.assert_not_called()

    def test_incomplete_backup_or_rehearsal_is_never_replayed(self):
        verified = {'phase': 'verified', 'restore': {'state': 'removed'}, 'archives': {}}
        for backup, rehearsal in (({'phase': 'capturing'}, {}),
                                  (verified, {}),
                                  (verified, {'state': 'creating'}),
                                  (verified, {'state': 'completed', 'cleanup': 'uncertain-no-original-ID'})):
            item = bare(record={'backup': backup, 'rehearsal': rehearsal}, directory=Mock())
            item.deadline, item.retained, item.fenced_source = Mock(), Mock(), Mock()
            with self.subTest(backup=backup, rehearsal=rehearsal), \
                    patch.object(upgrade, 'Restore') as restore, self.assertRaises(ValueError):
                item.backup()
            restore.assert_not_called()
            item.fenced_source.assert_not_called()

    def test_existing_operation_cannot_select_other_approvals_or_publication(self):
        record = {'version': upgrade.VERSION, 'parentSHA256': 'parent', 'chartSHA256': 'chart',
                  'selection': {'run': 1, 'revision': 'a'*40, 'images': {'migrator': {'digest': 'sha256:'+'b'*64}}},
                  'approvals': {'provenance': {'expiresAt': 1}, 'release': {'expiresAt': 1}}}
        source = SimpleNamespace(record={'selection': {'catalog': [], 'revision': 'r'}, 'chartSHA256': 'c'},
                                 parent=None, pause=None, digest=lambda: 'parent', close=Mock())
        for run, revision, files in ((2, 'a'*40, None), (1, 'c'*40, None),
                                     (None, None, ({'expiresAt': 2}, {'expiresAt': 1}))):
            config = SimpleNamespace(state_dir=Mock())
            config.state_dir.__truediv__ = Mock(return_value=SimpleNamespace(exists=lambda: True, is_symlink=lambda: False))
            def bounded(path, limit, files=files):
                if path == 'trust':
                    return files[0]
                if path == 'target':
                    return files[1]
                return record
            with self.subTest(run=run, revision=revision, files=files), \
                    patch.object(upgrade, 'CompletedFeedbackSource', return_value=source), \
                    patch.object(upgrade.browser_upgrade_chart, 'OriginalChart', return_value=Mock()), \
                    patch.object(upgrade.browser_upgrade_chart, 'digest', return_value='chart'), \
                    patch.object(upgrade.files, 'bounded_json', side_effect=bounded), \
                    self.assertRaises(ValueError):
                upgrade.ReviewRepairUpgrade(config, run, revision,
                                            'trust' if files else None, 'target' if files else None)


class RepairCLI(unittest.TestCase):
    def test_repair_requires_both_reviewed_documents_and_status_accepts_none(self):
        for argv in (['review-repair', '--cluster', 'c', '--provenance-trust', 'p'],
                     ['review-repair', '--cluster', 'c', '--release-target', 'r'],
                     ['review-repair-status', '--cluster', 'c', '--provenance-trust', 'p'],
                     ['review-repair-status', '--cluster', 'c', '--release-target', 'r']):
            with self.subTest(argv=argv), \
                    patch.object(runtime_upgrade_pause.platform, 'lock_state') as lock, \
                    self.assertRaises(ValueError):
                runtime_upgrade_pause.main(argv)
            lock.assert_not_called()


if __name__ == '__main__':
    unittest.main()
