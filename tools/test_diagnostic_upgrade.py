"""The diagnostic update cannot broaden its image, policy or intake effects."""
import copy
import unittest

import diagnostic_upgrade_target as target
import initial_activation
import install_images
import runtime_upgrade_pause
import test_initial_activation as fixture
import test_release_helm


class DiagnosticTests(unittest.TestCase):
    def source(self):
        values = fixture.source_values()
        values['components'] = {key: {'digest': 'sha256:' + 'a' * 64}
                                for key in install_images.COMPONENTS}
        values['migrator']['digest'] = 'sha256:' + 'a' * 64
        values['migrator']['enabled'] = False
        values['runtime']['intakePaused'] = True
        render = test_release_helm.ReleaseChartTests().render
        images = {'image': {'repository': install_images.REGISTRY, 'tag': '1' * 40},
                  'components': copy.deepcopy(values['components']), 'migrator': {'digest': 'sha256:' + 'a' * 64}}
        source = {'values': values, 'images': images, 'documents': render(values), 'externalRuntime': {}}
        publication = {component: {'digest': 'sha256:' + 'a' * 64} for component in install_images.COMPONENTS.values()}
        publication['migrator'] = {'digest': 'sha256:' + 'a' * 64}
        plan = {'source': {'migrations': [{}] * 33, 'images': publication, 'publication': 'source-run', 'revision': '1' * 40},
                'target': {'migrations': [{}] * 34, 'images': {'worker': {'digest': 'sha256:' + 'b' * 64},
                           'migrator': {'digest': 'sha256:' + 'c' * 64}}, 'publication': 'target-run', 'revision': '2' * 40},
                'pendingMigrations': [{'filename': '0034_execution_failures.sql', 'sha256': target.MIGRATION_SHA256}]}
        return source, plan, render

    def test_real_chart_changes_only_worker_and_verifier_gateway_before_resuming(self):
        source, plan, render = self.source()
        original = copy.deepcopy(source)
        result, selection = target.targets(source, plan, render, fixture.compiler(source['values']))
        self.assertEqual(source, original)
        self.assertFalse(selection['uniformTargetPublication'])
        self.assertEqual(selection['changedContainers'], [['ai-sdlc-verifier', 'model-gateway'], ['ai-sdlc-worker', 'model-gateway'], ['ai-sdlc-worker', 'worker']])
        self.assertEqual(result['staged']['values']['provenance'], source['values']['provenance'])
        self.assertEqual(result['staged']['values']['release'], source['values']['release'])
        self.assertEqual(result['staged']['values']['candidate'], source['values']['candidate'])
        self.assertEqual(result['staged']['values']['review'], source['values']['review'])
        for phase in ('staged', 'warming', 'armed', 'active'):
            docs = result[phase]['documents']
            deployments = [d for d in docs if d['kind'] == 'Deployment']
            self.assertEqual(len(deployments), 10)
            self.assertFalse(any(d['kind'] == 'Job' for d in docs))
            intake = next(d for d in deployments if d['metadata']['name'] == 'ai-sdlc-intake')
            self.assertEqual(intake['spec']['replicas'], int(phase == 'active'))
            if phase == 'staged':
                self.assertEqual({d['spec']['replicas'] for d in deployments}, {0})

    def test_unrelated_resource_image_or_policy_change_is_rejected(self):
        source, plan, render = self.source()
        for fault in ('image', 'policy', 'inventory'):
            with self.subTest(fault=fault):
                def changed(values):
                    docs = render(values)
                    if values['components']['worker']['digest'] == 'sha256:' + 'b' * 64:
                        if fault == 'inventory': docs.pop()
                        elif fault == 'image':
                            doc = next(d for d in docs if d['kind'] == 'Deployment' and d['metadata']['name'] == 'ai-sdlc-deployer')
                            doc['spec']['template']['spec']['containers'][0]['image'] = 'foreign'
                        else:
                            doc = next(d for d in docs if d['kind'] == 'ConfigMap')
                            doc.setdefault('data', {})['unexpected'] = 'authority-change'
                    return docs
                with self.assertRaises(ValueError):
                    target.targets(source, plan, changed, fixture.compiler(source['values']))

    def test_changed_original_publication_or_non_diagnostic_migration_is_rejected(self):
        source, plan, render = self.source()
        for fault in ('source-image', 'migration'):
            value = copy.deepcopy(plan)
            if fault == 'source-image': value['source']['images']['intake']['digest'] = 'sha256:' + 'e' * 64
            else: value['pendingMigrations'][0]['filename'] = 'unreviewed.sql'
            with self.subTest(fault=fault), self.assertRaises(ValueError):
                target.targets(source, value, render, fixture.compiler(source['values']))


if __name__ == '__main__':
    unittest.main()
