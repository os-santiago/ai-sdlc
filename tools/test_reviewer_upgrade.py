"""Reviewer-only chart changes and retained-history administrative gates."""
import copy
import json
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import Mock, patch

import approval_window
import candidate_install
import install
import install_images
import reviewer_image_upgrade
import reviewer_upgrade_targets
import test_initial_activation
import test_tool_budget_upgrade
import tool_budget_targets


class ReviewerTargets(unittest.TestCase):
    def source(self):
        source, render = test_tool_budget_upgrade.TargetTests().source()
        compiler = test_initial_activation.compiler(source['values'])
        source = tool_budget_targets.targets(source, render, compiler)['active']
        lock = install.LOCAL / 'images.lock.json'
        evidence = json.loads(lock.with_name('images.evidence.json').read_text())
        evidence['images']['verifier'] = dict(evidence['images']['worker'], component='verifier')
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'images.lock.json'
            path.with_name('images.evidence.json').write_text(json.dumps(evidence))
            desired = candidate_install.desired_values(install.ROOT, path, install.image_lock(lock), verification=True, review=True)
        source['values']['review'] = desired['review']
        source['documents'] = render(source['values'])
        return source, render, compiler

    def test_actual_chart_changes_only_reviewer_image_and_intake_opens_last(self):
        source, render, compiler = self.source()
        original = copy.deepcopy(source)
        image = install_images.REGISTRY + '@sha256:' + 'c' * 64
        result = reviewer_upgrade_targets.targets(source, image, render, compiler)
        config = json.loads(result['active']['values']['review']['configJSON'])
        self.assertEqual(config['workerImage'], image)
        config['workerImage'] = json.loads(source['values']['review']['configJSON'])['workerImage']
        self.assertEqual(config, json.loads(source['values']['review']['configJSON']))
        self.assertEqual(source, original)
        restored = copy.deepcopy(result['active']['values'])
        restored['review'] = source['values']['review']
        self.assertEqual(render(restored), source['documents'])
        for phase in ('paused', 'staged', 'warming', 'armed', 'active'):
            self.assertEqual(result[phase]['images'], source['images'])
            self.assertEqual(result[phase]['values']['candidate'], source['values']['candidate'])
            self.assertEqual(result[phase]['values']['release'], source['values']['release'])
            self.assertEqual(result[phase]['values']['provenance'], source['values']['provenance'])
            deployments = [d for d in result[phase]['documents'] if d['kind'] == 'Deployment']
            self.assertEqual(len(deployments), 10)
            intake = next(d for d in deployments if d['metadata']['name'] == 'ai-sdlc-intake')
            self.assertEqual(intake['spec']['replicas'], int(phase == 'active'))

    def test_review_tool_allowlist_is_inherited_as_recorded(self):
        # Installations recorded before the setting render unchanged and never
        # gain it; newer ones keep it in every phase.
        image = install_images.REGISTRY + '@sha256:' + 'c' * 64
        for recorded in (False, True):
            with self.subTest(recorded=recorded):
                source, render, compiler = self.source()
                if not recorded:
                    source['values']['review'].pop('narrowModelTools')
                    source['documents'] = render(source['values'])
                result = reviewer_upgrade_targets.targets(source, image, render, compiler)
                for phase in ('paused', 'staged', 'warming', 'armed', 'active'):
                    verifier = next(d for d in result[phase]['documents'] if d['kind'] == 'Deployment'
                                    and d['metadata']['name'] == 'ai-sdlc-verifier')
                    gateway = next(c for c in verifier['spec']['template']['spec']['containers'] if c['name'] == 'model-gateway')
                    names = [e['name'] for e in gateway['env']]
                    self.assertEqual('AI_SDLC_MODEL_GATEWAY_TOOLS' in names, recorded)

    def test_unpinned_foreign_and_unchanged_images_rejected(self):
        source, render, compiler = self.source()
        for image in ('latest', 'another/image@sha256:' + 'c'*64,
                      install_images.REGISTRY + ':main',
                      json.loads(source['values']['review']['configJSON'])['workerImage']):
            with self.subTest(image=image), self.assertRaises(ValueError):
                reviewer_upgrade_targets.targets(source, image, render, compiler)


class ReviewerOrdering(unittest.TestCase):
    def test_pending_work_prevents_snapshot_and_backup(self):
        item = object.__new__(reviewer_image_upgrade.ReviewerImageUpgrade)
        database = Mock()
        with patch.object(reviewer_image_upgrade.reviewer_upgrade_history, 'quiet', side_effect=ValueError('pending')):
            with self.assertRaises(ValueError): item.snapshot(database)
        database.snapshot.assert_not_called()

    def test_residual_pod_prevents_first_pause_capture(self):
        item = object.__new__(reviewer_image_upgrade.ReviewerImageUpgrade)
        item.observe = Mock(side_effect=ValueError('residual pod'))
        with patch.object(approval_window.ToolBudgetUpgrade, 'capture_first_pause') as capture:
            with self.assertRaises(ValueError): item.capture_first_pause(Mock())
            capture.assert_not_called()

    def test_existing_operation_never_refreshes_authority(self):
        item = object.__new__(reviewer_image_upgrade.ReviewerImageUpgrade)
        item.record = {'expiresAt': time.time()-1}
        item.save = Mock()
        item.begin()
        item.save.assert_not_called()
        with self.assertRaises(ValueError): item.deadline()

    def test_publication_cannot_change_schema_or_use_floating_scc(self):
        source = Mock()
        source.catalog.return_value = [{'filename': 'original'}]
        selected = {'runURL': 'authenticated-publication', 'images': {'worker': {'digest': 'sha256:'+'c'*64}, 'migrator': {'digest': 'sha256:'+'d'*64}}}
        with patch.object(reviewer_image_upgrade.install_images, 'resolve', return_value=({}, selected)):
            with self.assertRaises(ValueError):
                reviewer_image_upgrade.publication(source, Mock(), 42, 'a'*40, [], 'b'*40)
            for text in (b'ARG SCC_ENGINE_PIN=main\n', b'ARG SCC_ENGINE_PIN=v1.0\n', b''):
                source.git.return_value = text
                with self.assertRaises(ValueError):
                    reviewer_image_upgrade.publication(source, Mock(), 42, 'a'*40, source.catalog.return_value, 'b'*40)
            source.git.return_value = ('ARG SCC_ENGINE_PIN='+'c'*40+'\n').encode()
            result = reviewer_image_upgrade.publication(source, Mock(), 42, 'a'*40, source.catalog.return_value, 'b'*40)
            self.assertEqual(result['sccRevision'], 'c'*40)
            self.assertEqual(result['workerImage'], install_images.REGISTRY+'@sha256:'+'c'*64)


if __name__ == '__main__':
    unittest.main()
