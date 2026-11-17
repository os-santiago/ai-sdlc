"""Closed chart delta and fail-closed feedback migration boundaries."""
import copy
import hashlib
import json
import time
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import install_images
import prepared_resources
import review_feedback_runtime_upgrade as upgrade
import review_feedback_upgrade_targets as targets
import review_feedback_schema
import test_reviewer_upgrade


class FeedbackTargets(unittest.TestCase):
    def source(self):
        source, render, compiler = test_reviewer_upgrade.ReviewerTargets().source()
        components = install_images.ARTIFACT_COMPONENTS | {'artifact-builder', 'build-controller', 'deployer', 'browser-verifier'}
        selection = {'images': {name: {'digest': 'sha256:' + hashlib.sha256(name.encode()).hexdigest()} for name in components}}
        trust = json.loads(source['values']['provenance']['trustJSON'])
        trust['verifierImage'] = install_images.REGISTRY + '@' + selection['images']['verifier']['digest']
        return source, render, compiler, selection, trust

    def test_real_chart_changes_only_two_verifier_images_and_keeps_intake_last(self):
        source, render, compiler, selection, trust = self.source()
        original = copy.deepcopy(source)
        result = targets.targets(source, selection, trust, render, render, compiler)
        self.assertEqual(source, original)
        before = prepared_resources.documents(source['documents'])
        after = prepared_resources.documents(result['active']['documents'])
        image_changes = []
        for key in before:
            if before[key]['kind'] == 'Deployment':
                left = before[key]['spec']['template']['spec']['containers']
                right = after[key]['spec']['template']['spec']['containers']
                if [x['image'] for x in left] != [x['image'] for x in right]:
                    image_changes.append(before[key]['metadata']['name'])
        self.assertEqual(sorted(image_changes), ['ai-sdlc-provenance-verifier', 'ai-sdlc-verifier'])
        for phase in ('paused', 'staged', 'warming', 'armed', 'active'):
            values = result[phase]['values']
            for field in ('candidate', 'review', 'release'):
                self.assertEqual(values[field], result['paused']['values'][field] if phase in ('paused', 'staged') else source['values'][field])
            deployments = [obj for obj in result[phase]['documents'] if obj['kind'] == 'Deployment']
            self.assertEqual(len(deployments), 10)
            intake = next(obj for obj in deployments if obj['metadata']['name'] == 'ai-sdlc-intake')
            self.assertEqual(intake['spec']['replicas'], int(phase == 'active'))
            if phase in ('paused', 'staged'):
                self.assertEqual({obj['spec']['replicas'] for obj in deployments}, {0})

    def test_same_inventory_chart_drift_rejected(self):
        source, render, compiler, selection, trust = self.source()
        def changed(values):
            docs = render(values)
            deployment = next(obj for obj in docs if obj['kind'] == 'Deployment')
            deployment['spec']['template']['spec']['containers'][0].setdefault('env', []).append({'name': 'UNREVIEWED', 'value': '1'})
            return docs
        with self.assertRaises(ValueError):
            targets.targets(source, selection, trust, render, changed, compiler)

    def test_trust_cannot_renew_window_or_change_scope(self):
        source, render, compiler, selection, trust = self.source()
        for field, value in (('expiresAt', trust.get('expiresAt', 0)+1), ('publisherImage', 'different'), ('registryCA', 'different')):
            with self.subTest(field=field), self.assertRaises(ValueError):
                targets.targets(source, selection, dict(trust, **{field: value}), render, render, compiler)


class FeedbackGuards(unittest.TestCase):
    def test_equal_catalog_does_not_authorize_another_checkout_or_dirty_tools(self):
        source = Mock()
        source.git.side_effect = [b'reviewed\n', b'']
        upgrade.exact_local_publication(source, 'reviewed')
        for observations in ([b'older\n'], [b'reviewed\n', b' M tools/review_feedback_runtime_upgrade.py\n'],
                             [b'reviewed\n', b'?? unreviewed.py\n']):
            source.git.side_effect = observations
            with self.subTest(observations=observations), self.assertRaises(ValueError):
                upgrade.exact_local_publication(source, 'reviewed')

    def test_residual_browser_prevents_first_journal_or_effect(self):
        item = object.__new__(upgrade.ReviewFeedbackUpgrade)
        item.record = None
        item.selection = {'images': {}}
        item.save, item.apply, item.database = Mock(), Mock(), Mock()
        observer = Mock()
        uid = '11111111-1111-4111-8111-111111111111'
        observer.get.side_effect = [{'metadata': {'uid': uid}}, {'items': [{'kind': 'Pod'}]}]
        item.observe = Mock(side_effect=lambda phase: upgrade.no_browser_execution(observer, uid))
        with patch.object(upgrade.build_runtime_install, 'component_call') as effect, self.assertRaises(ValueError):
            item.begin()
        effect.assert_not_called()
        item.save.assert_not_called()
        item.apply.assert_not_called()
        item.database.assert_not_called()

    def test_chart_drift_prevents_any_custody_effect(self):
        item = object.__new__(upgrade.ReviewFeedbackUpgrade)
        item.target_chart = 'unread-test-path'
        item.source_chart = SimpleNamespace(expected='original')
        item.source_browser, item.save, item.apply = Mock(), Mock(), Mock()
        with patch.object(upgrade.browser_upgrade_chart, 'digest', return_value='changed'), self.assertRaises(ValueError):
            item.custody()
        item.source_browser.custody.assert_not_called()
        item.save.assert_not_called()
        item.apply.assert_not_called()

    def test_browser_inventory_requires_original_namespace_and_no_residue(self):
        uid = '11111111-1111-4111-8111-111111111111'
        namespace = {'metadata': {'uid': uid}}
        observer = Mock()
        observer.get.side_effect = [namespace, {'items': []}, namespace]
        upgrade.no_browser_execution(observer, uid)
        self.assertEqual(observer.get.call_count, 3)
        for response in ({'items': [{'kind': 'Pod'}]}, {'items': [{'kind': 'Job'}]}, {'items': [{'kind': 'CronJob'}]}, {}, None):
            observer = Mock()
            observer.get.side_effect = [namespace, response]
            with self.subTest(response=response), self.assertRaises(ValueError):
                upgrade.no_browser_execution(observer, uid)
        for position in (0, 2):
            objects = [namespace, {'items': []}, namespace]
            objects[position] = {'metadata': {'uid': '22222222-2222-4222-8222-222222222222'}}
            observer.get.side_effect = objects
            with self.assertRaises(ValueError):
                upgrade.no_browser_execution(observer, uid)

    def test_source_metadata_is_original_not_current_baseline(self):
        item = object.__new__(upgrade.ReviewFeedbackUpgrade)
        fields = ('@schema', '@roles', '@membership', '@role-settings', 'schema_migrations')
        original = dict.fromkeys(fields, 'original')
        item.source_browser = SimpleNamespace(record={'migratedProof': original, 'sourceRoleIdentities': 'roles'})
        item.save, item.apply = Mock(), Mock()
        for field in fields:
            with self.subTest(field=field), self.assertRaises(ValueError):
                item.original_database_authority(dict(original, **{field: 'drift'}), Mock())
        item.save.assert_not_called()
        item.apply.assert_not_called()

    def test_expired_original_intent_cannot_be_renewed(self):
        item = object.__new__(upgrade.ReviewFeedbackUpgrade)
        item.record = {'expiresAt': time.time()-1}
        item.save, item.apply = Mock(), Mock()
        with self.assertRaises(ValueError):
            item.run()
        item.save.assert_not_called()
        item.apply.assert_not_called()

    def test_unknown_migration_is_not_replayed(self):
        item = object.__new__(upgrade.ReviewFeedbackUpgrade)
        item.record = {'expiresAt': time.time()+60, 'migration': {'phase': 'sent'}}
        item.observe = Mock(return_value=({}, 'unused'))
        item.retained, item.save, item.config = Mock(), Mock(), Mock()
        with patch.object(upgrade, 'Migrator') as migrator, self.assertRaises(ValueError):
            item.migrate()
        migrator.assert_not_called()
        item.config.read.assert_not_called()
        item.save.assert_not_called()

    def test_feedback_descriptor_rejects_timestamp_acl_and_rls_drift(self):
        original = review_feedback_schema.EXPECTED
        for field, value in (('rls', False), ('forceRls', False), ('columnACLs', []), ('policies', [])):
            database = Mock()
            database.sql.return_value = json.dumps(dict(original, **{field: value}))
            with self.subTest(field=field), self.assertRaises(ValueError):
                review_feedback_schema.verify(database)


if __name__ == '__main__':
    unittest.main()
