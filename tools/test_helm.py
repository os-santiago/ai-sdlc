"""Test rendered installation contracts rather than template text."""
import json
import os
from pathlib import Path
import subprocess
import unittest
import yaml

ROOT = Path(__file__).resolve().parents[1]
CHART = ROOT / 'runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime'
LOCAL = ROOT / 'runtime/k8s/deploy/local/values.yaml'
HELM = os.environ.get('AI_SDLC_TEST_HELM', 'helm')


class ChartTests(unittest.TestCase):
    def render(self, *args):
        return subprocess.run([HELM, 'template', 'ai-sdlc', str(CHART), '--namespace', 'ai-sdlc-runtime', '-f', str(LOCAL), *args], capture_output=True, text=True)

    def test_independent_components_persistence_and_policy(self):
        result = self.render()
        self.assertEqual(result.returncode, 0, result.stderr)
        docs = [d for d in yaml.safe_load_all(result.stdout) if d]
        deployments = [d for d in docs if d['kind'] == 'Deployment']
        self.assertEqual(len(deployments), 5)
        services = {d['metadata']['name'] for d in docs if d['kind'] == 'Service'}
        for deployment in deployments:
            pod = deployment['spec']['template']['spec']
            self.assertEqual(len(pod['containers']), 1)
            self.assertFalse(pod['automountServiceAccountToken'])
            container = pod['containers'][0]
            self.assertTrue(container['securityContext']['readOnlyRootFilesystem'])
            self.assertFalse(container['securityContext']['allowPrivilegeEscalation'])
            self.assertIn(deployment['metadata']['name'], services)
            self.assertTrue(any(v['name'] == 'contract' for v in container['volumeMounts']))
            component = deployment['metadata']['labels']['app.kubernetes.io/component']
            database = next(e for e in container['env'] if e['name'] == 'DATABASE_URL')
            self.assertEqual(database['valueFrom']['secretKeyRef']['name'], 'ai-sdlc-database-' + component)
            identity = next(v for v in pod['volumes'] if v['name'] == 'identity')
            self.assertEqual(identity['secret']['secretName'], 'ai-sdlc-identity-' + component)
            self.assertFalse(any(v.get('secret', {}).get('secretName') == 'ai-sdlc-database-roles' for v in pod['volumes']))
        migrator = next(d for d in docs if d['kind'] == 'Job')
        pod = migrator['spec']['template']['spec']
        self.assertFalse(pod['automountServiceAccountToken'])
        self.assertEqual(pod['containers'][0]['args'], ['migrate'])
        self.assertEqual(migrator['spec']['activeDeadlineSeconds'], 300)
        postgres = next(d for d in docs if d['kind'] == 'StatefulSet')
        self.assertEqual(postgres['spec']['persistentVolumeClaimRetentionPolicy']['whenDeleted'], 'Retain')
        config = next(d['data'] for d in docs if d['kind'] == 'ConfigMap' and 'AI_SDLC_RELAY_ROUTES' in d.get('data', {}))
        self.assertEqual(config['AI_SDLC_RUNTIME_MODE'], 'maintenance')
        for url in json.loads(config['AI_SDLC_RELAY_ROUTES']).values():
            self.assertNotIn('localhost', url)
            self.assertIn(url.split('/')[2].split(':')[0], services)
        self.assertFalse(any(d['kind'] == 'Secret' for d in docs))

    def test_unsupported_live_mode_and_conflicting_databases_fail(self):
        for args in (['--set', 'runtime.mode=active'], ['--set', 'postgres.cloudNativePG.enabled=true'], ['--set', 'runtime.typo=true']):
            self.assertNotEqual(self.render(*args).returncode, 0)

    def test_published_lock_uses_exact_digests(self):
        lock = ROOT / 'runtime/k8s/deploy/local/images.lock.json'
        if not lock.exists(): self.skipTest('Image publication not completed yet')
        result = self.render('-f', str(lock))
        self.assertEqual(result.returncode, 0, result.stderr)
        docs = [d for d in yaml.safe_load_all(result.stdout) if d and d['kind'] == 'Deployment']
        for doc in docs:
            image = doc['spec']['template']['spec']['containers'][0]['image']
            self.assertRegex(image, r'^quay.io/os-santiago/ai-sdlc@sha256:[a-f0-9]{64}$')


if __name__ == '__main__': unittest.main()
