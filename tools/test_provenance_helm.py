"""Inspect rendered provenance custody, routing and additive network policies."""
import json
import unittest
import yaml
import test_helm


class ProvenanceChartTests(unittest.TestCase):
    def test_paused_preparation_excludes_all_delivery_components(self):
        values = self.provenance_values()
        values['runtime']['paused'] = True
        for section, key in (('authority', 'policyJSON'), ('build', 'outerPolicyJSON')):
            policy = json.loads(values[section][key]); policy['freeze'] = True
            values[section][key] = json.dumps(policy)
        config = json.loads(values['build']['configJSON'])
        config['inner'] = json.loads(values['authority']['policyJSON'])
        config['outer'] = json.loads(values['build']['outerPolicyJSON'])
        values['build']['configJSON'] = json.dumps(config)
        result = self.render_build(values)
        self.assertEqual(result.returncode, 0, result.stderr)
        docs = [d for d in yaml.safe_load_all(result.stdout) if d]
        deployments = [d for d in docs if d['kind'] == 'Deployment']
        self.assertEqual(len(deployments), 9)
        self.assertTrue(all(d['spec']['replicas'] == 0 for d in deployments))
        postgres = next(d for d in docs if d['kind'] == 'StatefulSet')
        self.assertEqual(postgres['spec']['replicas'], 1)
        self.assertTrue(any(d['kind'] == 'Job' for d in docs))

    def test_paused_preparation_rejects_unfrozen_or_absent_authority(self):
        values = self.provenance_values()
        values['runtime']['paused'] = True
        self.assertNotEqual(self.render_build(values).returncode, 0)
        policy = json.loads(values['authority']['policyJSON']); policy['freeze'] = True
        values['authority']['policyJSON'] = json.dumps(policy)
        config = json.loads(values['build']['configJSON']); config['inner'] = policy
        values['build']['configJSON'] = json.dumps(config)
        self.assertNotEqual(self.render_build(values).returncode, 0)
        self.assertNotEqual(self.render('--set', 'runtime.paused=true').returncode, 0)

    render = test_helm.ChartTests.render
    render_build = test_helm.ChartTests.render_build
    build_values = test_helm.ChartTests.build_values
    def provenance_values(self):
        values = self.build_values()
        build = json.loads(values['build']['configJSON'])
        build['profile'].update(clusterUID='11111111-1111-1111-1111-111111111111',
                                namespaceUID='22222222-2222-2222-2222-222222222222',
                                publisherImage='quay.io/os-santiago/ai-sdlc@sha256:'+'c'*64)
        values['build']['configJSON'] = json.dumps(build)
        # Shape fixture only: runtime ParseTrust validates actual CA/key/time.
        trust = dict(version='ai-sdlc.saferus-provenance/v1', scope='kind-test-production',
                     repository='os-santiago/saferus', environment='ai-sdlc-production',
                     clusterUID=build['profile']['clusterUID'], buildNamespaceUID=build['profile']['namespaceUID'],
                     publisherImage=build['profile']['publisherImage'],
                     verifierImage='quay.io/os-santiago/ai-sdlc@sha256:'+'a'*64,
                     registryCA='shape-only-ca', signerPublicKey='shape-only-key')
        values['provenance'] = dict(enabled=True, trustJSON=json.dumps(trust),
                                    registryCredentialSecret='ai-sdlc-provenance-registry', credentialRevision='d'*64)
        return values

    def test_dedicated_process_has_no_kubernetes_or_model_custody(self):
        for shared in ('true', 'false'):
            result = self.render_build(self.provenance_values(), '--set', 'networkPolicy.enabled='+shared,
                                       '--set', 'namespace.name=effective-runtime')
            self.assertEqual(result.returncode, 0, result.stderr)
            docs = [d for d in yaml.safe_load_all(result.stdout) if d]
            deployment = next(d for d in docs if d['kind']=='Deployment' and d['metadata']['name']=='ai-sdlc-provenance-verifier')
            self.assertEqual(deployment['metadata']['namespace'], 'effective-runtime')
            pod = deployment['spec']['template']['spec']; container = pod['containers'][0]
            self.assertEqual(len(pod['containers']), 1)
            self.assertEqual(container['args'], ['provenance'])
            self.assertEqual(container['image'], 'quay.io/os-santiago/ai-sdlc@sha256:'+'a'*64)
            self.assertFalse(pod['automountServiceAccountToken'])
            self.assertEqual(pod['serviceAccountName'], 'ai-sdlc-provenance-verifier')
            self.assertTrue(container['securityContext']['readOnlyRootFilesystem'])
            self.assertEqual({v['secret']['secretName'] for v in pod['volumes'] if 'secret' in v},
                             {'ai-sdlc-identity-verifier', 'ai-sdlc-provenance-registry'})
            self.assertFalse(any('projected' in v for v in pod['volumes']))
            for binding in [d for d in docs if d['kind'] in ('RoleBinding', 'ClusterRoleBinding')]:
                self.assertNotIn('ai-sdlc-provenance-verifier', [s['name'] for s in binding.get('subjects', [])])
            for d in [d for d in docs if d['kind']=='Deployment' and d is not deployment]:
                self.assertNotIn('ai-sdlc-provenance-registry',
                                 [v['secret']['secretName'] for v in d['spec']['template']['spec'].get('volumes', []) if 'secret' in v])
            config = next(d['data'] for d in docs if d['kind']=='ConfigMap' and 'AI_SDLC_RELAY_ROUTES' in d.get('data', {}))
            self.assertEqual(config['AI_SDLC_PROVENANCE'], 'true')
            self.assertEqual(json.loads(config['AI_SDLC_RELAY_ROUTES'])['verify-built'],
                             'http://ai-sdlc-provenance-verifier:8089/internal/verify-built')
            policy = next(d for d in docs if d['kind']=='NetworkPolicy' and d['metadata']['name']=='ai-sdlc-provenance-verifier')
            self.assertEqual(policy['metadata']['namespace'], 'effective-runtime')
            self.assertEqual(policy['spec']['ingress'][0]['from'][0]['podSelector']['matchLabels']['app.kubernetes.io/component'], 'relay')
            egress = policy['spec']['egress']
            self.assertEqual({p['port'] for rule in egress for p in rule['ports']}, {53, 5432, 5000})
            self.assertFalse(any('ipBlock' in peer for rule in egress for peer in rule['to']))
            registry = next(rule for rule in egress if rule['ports'][0]['port']==5000)
            self.assertEqual(registry['to'], [{'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name':'kind-registry'}},
                                               'podSelector': {'matchLabels': {'run':'registry'}}}])
            if shared=='true':
                general = next(d for d in docs if d['kind']=='NetworkPolicy' and d['metadata']['name']=='ai-sdlc-internal')
                self.assertIn({'key':'app.kubernetes.io/component','operator':'NotIn','values':['provenance-verifier']},general['spec']['podSelector']['matchExpressions'])

    def test_provenance_cannot_render_with_partial_or_substituted_authority(self):
        for fault in ('build', 'review', 'merge', 'publication', 'verifier', 'credential', 'revision', 'trust', 'image', 'cluster', 'publisher'):
            values = self.provenance_values()
            if fault in ('build', 'review', 'merge', 'publication'): values[fault]['enabled'] = False
            elif fault=='verifier': values['verifier']['digest'] = ''
            elif fault=='credential': values['provenance']['registryCredentialSecret'] = ''
            elif fault=='revision': values['provenance']['credentialRevision'] = ''
            elif fault=='trust': values['provenance']['trustJSON'] = '{}'
            else:
                trust=json.loads(values['provenance']['trustJSON'])
                trust[{'image':'verifierImage','cluster':'clusterUID','publisher':'publisherImage'}[fault]]='substituted'
                values['provenance']['trustJSON']=json.dumps(trust)
            self.assertNotEqual(self.render_build(values).returncode, 0, fault)

    def test_registry_secret_cannot_alias_existing_custody(self):
        for name in ('ai-sdlc-identity-verifier', 'ai-sdlc-database-verifier', 'ai-sdlc-github-client', 'ai-sdlc-database-roles', 'arbitrary-secret'):
            values=self.provenance_values();values['provenance']['registryCredentialSecret']=name
            self.assertNotEqual(self.render_build(values).returncode,0,name)
        for target in ('migrator', 'intake', 'database', 'model'):
            values=self.provenance_values();reserved='ai-sdlc-provenance-registry'
            if target=='migrator': values['migrator']={'credentialsSecret':reserved}
            elif target=='intake': values['intake']={'credentialSecret':reserved}
            elif target=='database': values['postgres']={'databaseURLSecret':{'name':reserved}}
            else: values['modelProvider']={'apiKeySecret':{'name':reserved}}
            self.assertNotEqual(self.render_build(values).returncode,0,target)

    def test_disabled_default_has_no_provenance_process_or_custody(self):
        result = self.render()
        self.assertEqual(result.returncode, 0, result.stderr)
        docs=[d for d in yaml.safe_load_all(result.stdout) if d]
        self.assertFalse(any('provenance' in d['metadata']['name'] for d in docs))
        config=next(d['data'] for d in docs if d['kind']=='ConfigMap' and 'AI_SDLC_RELAY_ROUTES' in d.get('data', {}))
        self.assertEqual(config['AI_SDLC_PROVENANCE'], 'false')
        self.assertNotIn('verify-built', json.loads(config['AI_SDLC_RELAY_ROUTES']))


if __name__=='__main__':
    unittest.main()
