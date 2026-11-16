import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import yaml
import candidate_install
import install
import install_config

class PublicationInstallTests(unittest.TestCase):
    def test_publication_requires_explicit_reviewed_scope(self):
        for args in (['local', '--publication'], ['local', '--publication', '--independent-review', '--profile', 'verification'],
                     ['local', '--publication', '--repository', 'os-santiago/homedir', '--profile', 'verification']):
            with patch.object(install, 'tools') as tools:
                with self.assertRaises(ValueError):
                    install.main(args)
                tools.assert_not_called()
        config = json.loads((install.ROOT / 'examples/homedir.local-install.json').read_text())
        config.update(profile='verification', independentReview=True)
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'config.json'
            path.write_text(json.dumps(config))
            original = install_config.load(path)[1]
            config['publication'] = True
            path.write_text(json.dumps(config))
            values, digest = install_config.load(path)
            self.assertTrue(values['publication'])
            self.assertNotEqual(original, digest)
            for change in ({'publication': 'true'}, {'independentReview': False}, {'repository': 'os-santiago/ai-sdlc'}, {'profile': 'candidate'}):
                path.write_text(json.dumps(dict(config, **change)))
                with self.assertRaises(ValueError):
                    install_config.load(path)

    def test_publisher_credential_import_is_separate_and_explicit(self):
        with patch.object(install, 'owned_secret', return_value=None), patch.object(install.github_client, 'configure') as configure:
            with self.assertRaises(ValueError):
                install.configure_publication_client(['kubectl'], 'fixture-uid', 'os-santiago/homedir', 'none')
            configure.assert_not_called()
            with patch.object(install, 'read_resource', return_value={'metadata': {'resourceVersion': '12'}}):
                self.assertEqual(install.configure_publication_client(['kubectl'], 'fixture-uid', 'os-santiago/homedir', 'gh-cli'), '12')
            self.assertEqual(configure.call_args.kwargs['name'], 'ai-sdlc-publication-client')
        with patch.object(install, 'identity_material') as material:
            with self.assertRaises(ValueError):
                install.configure_identities([], 'podman', {}, 'uid', 'os-santiago/homedir', 'kind', publication=True)
            material.assert_not_called()

    def render(self, enabled, **changes):
        values = {'runtime': {'mode': 'verification'}, 'verifier': {'digest': 'sha256:' + 'a'*64},
                  'review': {'enabled': True, 'configJSON': '{}'},
                  'publication': {'enabled': enabled, 'credentialRevision': '12'},
                  'authority': {'policyJSON': json.dumps({'repository': 'os-santiago/homedir'})}}
        values.update(changes)
        with tempfile.NamedTemporaryFile(mode='w', suffix='.json') as config:
            json.dump(values, config); config.flush()
            result = subprocess.run([os.environ.get('AI_SDLC_TEST_HELM', 'helm'), 'template', 'ai-sdlc', str(install.CHART),
                                     '--namespace', 'ai-sdlc-runtime', '-f', str(install.LOCAL / 'values.yaml'), '-f', config.name],
                                    text=True, capture_output=True)
        return result

    def test_chart_publication_is_exclusive_without_model_or_kubernetes_token(self):
        result = self.render(True)
        self.assertEqual(result.returncode, 0, result.stderr)
        docs = [doc for doc in yaml.safe_load_all(result.stdout) if doc]
        owners = []
        for doc in docs:
            if doc['kind'] != 'Deployment':
                continue
            pod = doc['spec']['template']['spec']
            secrets = {v.get('secret', {}).get('secretName') for v in pod.get('volumes', [])}
            if 'ai-sdlc-publication-client' in secrets:
                owners.append(doc['metadata']['name'])
                self.assertFalse(pod['automountServiceAccountToken'])
                self.assertFalse(any('projected' in v for v in pod['volumes']))
                self.assertEqual(len(pod['containers']), 1)
                self.assertFalse(secrets & {'ai-sdlc-model-client', 'ai-sdlc-codex-auth', 'ai-sdlc-review-model-client'})
                mounts = {v['mountPath']: v for v in pod['containers'][0]['volumeMounts']}
                for path in ('/etc/ai-sdlc/publication', '/etc/ai-sdlc/review', '/etc/ai-sdlc/execution'):
                    self.assertTrue(mounts[path]['readOnly'])
        self.assertEqual(owners, ['ai-sdlc-release-manager'])
        config = next(d['data'] for d in docs if d['kind'] == 'ConfigMap' and d['metadata']['name'] == 'ai-sdlc-config')
        routes = json.loads(config['AI_SDLC_RELAY_ROUTES'])
        self.assertEqual(len(routes), 5)
        self.assertEqual(routes['publish-candidate'], 'http://ai-sdlc-release-manager:8084/internal/publish-candidate')
        self.assertIn('verify-artifact', routes)
        policy = next(d for d in docs if d['kind'] == 'NetworkPolicy' and d['metadata']['name'] == 'ai-sdlc-publication-github')
        self.assertEqual(policy['spec']['podSelector']['matchLabels']['app.kubernetes.io/component'], 'release-manager')
        self.assertEqual(policy['spec']['egress'][0]['ports'], [{'protocol': 'TCP', 'port': 443}])

    def test_disabled_chart_revokes_mount_and_route_and_wrong_modes_fail(self):
        result = self.render(False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('ai-sdlc-publication-client', result.stdout)
        self.assertNotIn('/internal/publish-candidate', result.stdout)
        self.assertNotIn('name: ai-sdlc-publication-github', result.stdout)
        for changes in ({'runtime': {'mode': 'candidate'}}, {'review': {'enabled': False}},
                        {'authority': {'policyJSON': json.dumps({'repository': 'os-santiago/ai-sdlc'})}}):
            self.assertNotEqual(self.render(True, **changes).returncode, 0)

if __name__ == '__main__':
    unittest.main()
