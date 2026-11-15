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
        self.assertEqual(len(deployments), 6)
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
        docs = [d for d in yaml.safe_load_all(result.stdout) if d and d['kind'] in ('Deployment', 'Job')]
        self.assertEqual(len(docs), 7)
        for doc in docs:
            image = doc['spec']['template']['spec']['containers'][0]['image']
            self.assertRegex(image, r'^quay.io/os-santiago/ai-sdlc@sha256:[a-f0-9]{64}$')

    def test_shadow_github_credential_is_mounted_only_in_intake(self):
        result = self.render('-f', str(ROOT / 'runtime/k8s/deploy/local/profiles/shadow.json'))
        self.assertEqual(result.returncode, 0, result.stderr)
        docs = [d for d in yaml.safe_load_all(result.stdout) if d and d['kind'] == 'Deployment']
        for doc in docs:
            name = doc['metadata']['labels']['app.kubernetes.io/component']
            pod = doc['spec']['template']['spec']
            secrets = [v.get('secret', {}).get('secretName') for v in pod['volumes']]
            self.assertEqual('ai-sdlc-github-client' in secrets, name == 'intake')
            env = pod['containers'][0]['env']
            self.assertFalse(any(e['name'] == 'MODEL_API_KEY' for e in env))

    def test_public_https_egress_is_limited_to_shadow_intake(self):
        for profile in ('maintenance', 'shadow'):
            result = self.render('-f', str(ROOT / ('runtime/k8s/deploy/local/profiles/' + profile + '.json')))
            self.assertEqual(result.returncode, 0, result.stderr)
            policies = [d for d in yaml.safe_load_all(result.stdout) if d and d['kind'] == 'NetworkPolicy']
            public = [(p, rule) for p in policies for rule in p['spec'].get('egress', [])
                      if any(peer.get('ipBlock', {}).get('cidr') == '0.0.0.0/0' for peer in rule.get('to', []))]
            self.assertEqual(len(public), 1 if profile == 'shadow' else 0)
            for policy, rule in public:
                self.assertEqual(policy['spec']['podSelector']['matchLabels']['app.kubernetes.io/component'], 'intake')
                self.assertEqual(rule['ports'], [{'protocol': 'TCP', 'port': 443}])
                self.assertIn('169.254.0.0/16', rule['to'][0]['ipBlock']['except'])


if __name__ == '__main__': unittest.main()
