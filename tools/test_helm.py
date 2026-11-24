"""Test rendered installation contracts rather than template text."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import copy
import unittest
import yaml

ROOT = Path(__file__).resolve().parents[1]
CHART = ROOT / 'runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime'
LOCAL = ROOT / 'runtime/k8s/deploy/local/values.yaml'
HELM = os.environ.get('AI_SDLC_TEST_HELM', 'helm')


class ChartTests(unittest.TestCase):
    def render(self, *args):
        return subprocess.run([HELM, 'template', 'ai-sdlc', str(CHART), '--namespace', 'ai-sdlc-runtime', '-f', str(LOCAL), *args], capture_output=True, text=True)

    def build_values(self):
        inner = {'repository': 'os-santiago/saferus', 'environment': 'ai-sdlc-production', 'autonomy': 'auto-merge-low', 'freeze': False}
        outer = dict(inner, autonomy='auto-deploy')
        installed = {'version': 'ai-sdlc.build-installation/v1', 'inner': inner, 'outer': outer,
                     'profile': {'deniedTargets': [{'host': '10.96.0.1', 'port': 443}, {'host': '10.96.0.2', 'port': 443}]}}
        return {'runtime': {'mode': 'verification'}, 'verifier': {'digest': 'sha256:'+'a'*64},
                'review': {'enabled': True, 'configJSON': '{}'}, 'publication': {'enabled': True},
                'merge': {'enabled': True, 'configJSON': '{}'}, 'authority': {'policyJSON': json.dumps(inner)},
                'build': {'enabled': True, 'digest': 'sha256:'+'b'*64, 'configJSON': json.dumps(installed), 'outerPolicyJSON': json.dumps(outer)}}

    def render_build(self, values, *args):
        with tempfile.NamedTemporaryFile(mode='w', suffix='.json') as file:
            json.dump(values, file); file.flush()
            return self.render('-f', file.name, *args)

    def test_build_controller_has_only_original_workload_authority_and_private_identity(self):
        for global_policy in ('true', 'false'):
            result = self.render_build(self.build_values(), '--set', 'networkPolicy.enabled='+global_policy)
            self.assertEqual(result.returncode, 0, result.stderr)
            docs = [d for d in yaml.safe_load_all(result.stdout) if d]
            deployment = next(d for d in docs if d['kind']=='Deployment' and d['metadata']['name']=='ai-sdlc-build-controller')
            pod = deployment['spec']['template']['spec']; container = pod['containers'][0]
            self.assertEqual(len(pod['containers']), 1)
            self.assertEqual(pod['serviceAccountName'], 'ai-sdlc-build-controller')
            self.assertFalse(pod['automountServiceAccountToken'])
            self.assertTrue(container['securityContext']['readOnlyRootFilesystem'])
            self.assertEqual(container['image'], 'quay.io/os-santiago/ai-sdlc@sha256:'+'b'*64)
            self.assertEqual({v['secret']['secretName'] for v in pod['volumes'] if 'secret' in v}, {'ai-sdlc-identity-build-controller'})
            self.assertEqual(next(e for e in container['env'] if e['name']=='DATABASE_URL')['valueFrom']['secretKeyRef']['name'], 'ai-sdlc-database-build-controller')
            roles=[d for d in docs if d['kind'] in ('Role','ClusterRole') and d['metadata']['name'] in ('ai-sdlc-build-controller','ai-sdlc-build-inventory')]
            writes=[]
            for role in roles:
                for rule in role['rules']:
                    self.assertFalse(set(rule['verbs']) & {'patch','update','*','deletecollection'})
                    if set(rule['verbs']) & {'create','delete'}: writes.append((role['metadata']['namespace'],rule['resources']))
                    if 'secrets' in rule['resources']: self.assertEqual(rule['resourceNames'],['registry-auth'])
            self.assertEqual(writes,[('ai-sdlc-builds',['pipelineruns'])])
            policy=next(d for d in docs if d['kind']=='NetworkPolicy' and d['metadata']['name']=='ai-sdlc-build-controller')
            self.assertEqual(policy['spec']['ingress'][0]['from'][0]['podSelector']['matchLabels']['app.kubernetes.io/component'],'relay')
            self.assertEqual({p['ipBlock']['cidr'] for r in policy['spec']['egress'] for p in r['to'] if 'ipBlock' in p},{'10.96.0.1/32','10.96.0.2/32'})
            if global_policy=='true':
                general=next(d for d in docs if d['kind']=='NetworkPolicy' and d['metadata']['name']=='ai-sdlc-internal')
                self.assertIn({'key':'app.kubernetes.io/component','operator':'NotIn','values':['build-controller']},general['spec']['podSelector']['matchExpressions'])
            config=next(d['data'] for d in docs if d['kind']=='ConfigMap' and 'AI_SDLC_RELAY_ROUTES' in d.get('data',{}))
            self.assertEqual(config['AI_SDLC_BUILD'],'true')
            self.assertEqual(json.loads(config['AI_SDLC_RELAY_ROUTES'])['build-merged'],'http://ai-sdlc-build-controller:8088/internal/build-merged')

    def test_build_rejects_partial_authority_configuration(self):
        for fault in ('review','publication','merge','digest','outer','repository'):
            values=self.build_values()
            if fault in ('review','publication','merge'): values[fault]['enabled']=False
            elif fault=='digest': values['build']['digest']=''
            elif fault=='outer': values['build']['outerPolicyJSON']='{}'
            else: values['authority']['policyJSON']=json.dumps({'repository':'os-santiago/homedir'})
            self.assertNotEqual(self.render_build(values).returncode,0,fault)

    def test_verifier_effective_namespace_and_required_egress_without_global_policy(self):
        result = self.render('-f', str(ROOT / 'runtime/k8s/deploy/local/profiles/verification.json'),
                             '--set-string', 'verifier.digest=sha256:'+'a'*64,
                             '--set', 'namespace.name=effective-runtime', '--set', 'networkPolicy.enabled=false',
                             '--set', 'networkPolicy.databaseEgressCIDRs[0]=192.0.2.1/32')
        self.assertEqual(result.returncode, 0, result.stderr)
        docs = [d for d in yaml.safe_load_all(result.stdout) if d]
        for doc in docs:
            if doc['metadata']['name'] in ('ai-sdlc-verifier', 'ai-sdlc-verification-controller', 'ai-sdlc-verifier-egress'):
                self.assertEqual(doc['metadata']['namespace'], 'effective-runtime')
        binding = next(d for d in docs if d['kind']=='RoleBinding' and d['metadata']['namespace']=='ai-sdlc-verifiers')
        self.assertEqual(binding['subjects'][0]['namespace'], 'effective-runtime')
        policy = next(d for d in docs if d['kind']=='NetworkPolicy' and d['metadata']['name']=='ai-sdlc-verifier-egress')
        ports = {(p['protocol'],p['port']) for rule in policy['spec']['egress'] for p in rule['ports']}
        self.assertTrue({('UDP',53),('TCP',53),('TCP',5432)} <= ports)
        self.assertTrue(any(peer.get('ipBlock',{}).get('cidr')=='192.0.2.1/32'
                            for rule in policy['spec']['egress'] for peer in rule['to']))

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

    def test_candidate_isolates_gateway_credentials_and_executor_authority(self):
        result = self.render('-f', str(ROOT / 'runtime/k8s/deploy/local/profiles/candidate.json'),
                             '-f', str(ROOT / 'runtime/k8s/deploy/local/images.lock.json'))
        self.assertEqual(result.returncode, 0, result.stderr)
        docs = [d for d in yaml.safe_load_all(result.stdout) if d]
        worker = next(d for d in docs if d['kind'] == 'Deployment' and d['metadata']['name'] == 'ai-sdlc-worker')
        pod = worker['spec']['template']['spec']
        self.assertEqual(pod['serviceAccountName'], 'ai-sdlc-execution-controller')
        self.assertFalse(pod['automountServiceAccountToken'])
        controller, gateway = pod['containers']
        self.assertEqual(gateway['name'], 'model-gateway')
        self.assertEqual({v['name'] for v in gateway['volumeMounts']}, {'model-auth', 'model-home'})
        self.assertNotIn('model-auth', {v['name'] for v in controller['volumeMounts']})
        self.assertNotIn('DATABASE_URL', {v['name'] for v in gateway['env']})
        role = next(d for d in docs if d['kind'] == 'Role')
        self.assertEqual(role['metadata']['namespace'], 'ai-sdlc-executors')
        exec_rule = next(r for r in role['rules'] if r['resources'] == ['pods/exec'])
        self.assertEqual(set(exec_rule['verbs']), {'get', 'create'})
        for rule in role['rules']:
            self.assertFalse(set(rule['resources']) & {'secrets', '*', 'deployments'})
            self.assertFalse(set(rule['verbs']) & {'*', 'update', 'patch', 'list'})
        policy = next(d for d in docs if d['kind'] == 'NetworkPolicy' and d['metadata']['namespace'] == 'ai-sdlc-executors')
        self.assertEqual(policy['spec']['policyTypes'], ['Ingress', 'Egress'])
        self.assertNotIn('egress', policy['spec'])
        self.assertTrue(any(d['kind'] == 'ResourceQuota' for d in docs))

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

    def test_credential_refresh_replaces_only_worker_pod_template(self):
        templates = []
        for revision in ('101', '102'):
            result = self.render('-f', str(ROOT / 'runtime/k8s/deploy/local/profiles/candidate.json'),
                                 '--set-string', 'candidate.credentialRevision=' + revision)
            self.assertEqual(result.returncode, 0, result.stderr)
            docs = [d for d in yaml.safe_load_all(result.stdout) if d and d['kind'] == 'Deployment']
            templates.append({d['metadata']['name']: d['spec']['template'] for d in docs})
        changed = {name for name in templates[0] if templates[0][name] != templates[1][name]}
        self.assertEqual(changed, {'ai-sdlc-worker'})

    def test_verifier_is_separate_and_can_only_manage_offline_validation(self):
        self.assertNotEqual(self.render('--set', 'runtime.mode=verification').returncode, 0)
        result = self.render('--set', 'runtime.mode=verification', '--set', 'verifier.digest=sha256:' + 'a' * 64)
        self.assertEqual(result.returncode, 0, result.stderr)
        docs = [d for d in yaml.safe_load_all(result.stdout) if d]
        verifier = next(d for d in docs if d['kind'] == 'Deployment' and d['metadata']['name'] == 'ai-sdlc-verifier')
        pod = verifier['spec']['template']['spec']
        self.assertEqual(pod['serviceAccountName'], 'ai-sdlc-verification-controller')
        self.assertFalse(pod['automountServiceAccountToken'])
        self.assertEqual(len(pod['containers']), 1)
        secrets = {v.get('secret', {}).get('secretName') for v in pod['volumes']} - {None}
        self.assertEqual(secrets, {'ai-sdlc-identity-verifier'})
        for rule in next(d for d in docs if d['kind'] == 'Role' and d['metadata']['namespace'] == 'ai-sdlc-verifiers')['rules']:
            self.assertFalse(set(rule['resources']) & {'secrets', 'pods/exec', '*', 'deployments'})
            self.assertFalse(set(rule['verbs']) & {'*', 'patch', 'update'})
        policy = next(d for d in docs if d['kind'] == 'NetworkPolicy' and d['metadata']['namespace'] == 'ai-sdlc-verifiers')
        self.assertEqual(policy['spec'], {'podSelector': {}, 'policyTypes': ['Ingress', 'Egress']})
        config = next(d['data'] for d in docs if d['kind'] == 'ConfigMap' and 'AI_SDLC_RELAY_ROUTES' in d.get('data', {}))
        routes = json.loads(config['AI_SDLC_RELAY_ROUTES'])
        self.assertEqual(set(routes), {'admit-issue', 'plan-admission', 'execute-work', 'verify-artifact'})
        self.assertEqual(routes['verify-artifact'], 'http://ai-sdlc-verifier:8087/internal/verify-artifact')

    def sandbox_docs(self, values=None):
        values = dict(values or {}, runtime={'mode': 'verification'}, verifier={'digest': 'sha256:' + 'a' * 64})
        result = self.render_build(values)
        return result, [d for d in yaml.safe_load_all(result.stdout) if d] if result.returncode == 0 else []

    def test_sandbox_runtime_class_preflight_is_one_read_only_name(self):
        result, docs = self.sandbox_docs()
        self.assertEqual(result.returncode, 0, result.stderr)
        role = next(d for d in docs if d['kind'] == 'ClusterRole' and d['metadata']['name'] == 'ai-sdlc-sandbox-runtime-class')
        self.assertEqual(role['rules'], [{'apiGroups': ['node.k8s.io'], 'resources': ['runtimeclasses'],
                                          'resourceNames': ['ai-sdlc-offline'], 'verbs': ['get']}])
        binding = next(d for d in docs if d['kind'] == 'ClusterRoleBinding' and d['metadata']['name'] == 'ai-sdlc-sandbox-runtime-class')
        self.assertEqual(binding['roleRef'], {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'ClusterRole', 'name': 'ai-sdlc-sandbox-runtime-class'})
        self.assertEqual({(s['name'], s['namespace']) for s in binding['subjects']},
                         {('ai-sdlc-execution-controller', 'ai-sdlc-runtime'), ('ai-sdlc-verification-controller', 'ai-sdlc-runtime')})
        # Candidate binds only the worker; maintenance creates no sandbox pods.
        candidate = self.render('-f', str(ROOT / 'runtime/k8s/deploy/local/profiles/candidate.json'))
        self.assertEqual(candidate.returncode, 0, candidate.stderr)
        binding = next(d for d in yaml.safe_load_all(candidate.stdout) if d and d['kind'] == 'ClusterRoleBinding' and d['metadata']['name'] == 'ai-sdlc-sandbox-runtime-class')
        self.assertEqual([s['name'] for s in binding['subjects']], ['ai-sdlc-execution-controller'])
        maintenance = self.render()
        self.assertEqual(maintenance.returncode, 0, maintenance.stderr)
        self.assertNotIn('sandbox-runtime-class', maintenance.stdout)

    def test_configured_sandbox_runtime_class_must_match_installed_documents(self):
        kata = {'candidate': {'configJSON': json.dumps({'placement': {'RuntimeClass': 'kata'}})}}
        default = {'candidate': {'configJSON': json.dumps({'placement': {}})}}
        result, docs = self.sandbox_docs(dict(kata, sandbox={'runtimeClassName': 'kata'},
                                              review={'enabled': True, 'configJSON': json.dumps({'runtimeClass': 'kata'})}))
        self.assertEqual(result.returncode, 0, result.stderr)
        role = next(d for d in docs if d['kind'] == 'ClusterRole' and d['metadata']['name'] == 'ai-sdlc-sandbox-runtime-class')
        self.assertEqual(role['rules'][0]['resourceNames'], ['kata'])
        # Default documents omit the class; they match only the default value.
        result = self.sandbox_docs(dict(default, review={'enabled': True, 'configJSON': '{}'}))[0]
        self.assertEqual(result.returncode, 0, result.stderr)
        # The runtime matches field names case-insensitively; so does the chart.
        lower = {'candidate': {'configJSON': json.dumps({'placement': {'runtimeClass': 'kata'}})}}
        result = self.sandbox_docs(dict(lower, sandbox={'runtimeClassName': 'kata'},
                                        review={'enabled': True, 'configJSON': json.dumps({'RUNTIMECLASS': 'kata'})}))[0]
        self.assertEqual(result.returncode, 0, result.stderr)
        result = self.sandbox_docs({'candidate': {'configJSON': json.dumps({'Placement': {'RuntimeClass': 'kata'}})},
                                    'sandbox': {'runtimeClassName': 'kata'}})[0]
        self.assertEqual(result.returncode, 0, result.stderr)
        both = {'candidate': {'configJSON': json.dumps({'placement': {'RuntimeClass': 'kata', 'runtimeclass': 'kata'}})}}
        two_placements = {'candidate': {'configJSON': json.dumps({'placement': {}, 'PLACEMENT': {'RuntimeClass': 'kata'}})}}
        for values in (lower, dict(default, review={'enabled': True, 'configJSON': json.dumps({'RuntimeClass': 'kata'})}),
                       dict(both, sandbox={'runtimeClassName': 'kata'}),
                       dict(two_placements, sandbox={'runtimeClassName': 'kata'}), two_placements,
                       {'candidate': {'configJSON': json.dumps({'placement': []})}},
                       dict(default, sandbox={'runtimeClassName': 'kata'}), kata,
                       dict(kata, sandbox={'runtimeClassName': 'kata'}, review={'enabled': True, 'configJSON': '{}'}),
                       {'sandbox': {'runtimeClassName': 'Kata'}}, {'sandbox': {'runtimeClassName': 'kata/../x'}},
                       {'sandbox': {'runtimeClassName': ''}}, {'sandbox': {'runtimeClassName': None}}):
            with self.subTest(values=values):
                self.assertNotEqual(self.sandbox_docs(values)[0].returncode, 0)

    def test_sandbox_pod_overhead_adds_exact_quota_headroom(self):
        result, docs = self.sandbox_docs({'review': {'enabled': True, 'configJSON': '{}'},
                                          'sandbox': {'podOverhead': {'cpu': '250m', 'memory': '350Mi'}}})
        self.assertEqual(result.returncode, 0, result.stderr)
        quotas = {d['metadata']['name']: d['spec']['hard'] for d in docs if d['kind'] == 'ResourceQuota'}
        self.assertEqual(quotas['executor-bounds'], {'pods': '4', 'configmaps': '8', 'requests.cpu': '2000m', 'requests.memory': '2424Mi', 'limits.cpu': '7000m', 'limits.memory': '5496Mi'})
        self.assertEqual(quotas['verifier-bounds'], {'pods': '4', 'configmaps': '4', 'requests.cpu': '2000m', 'requests.memory': '2424Mi', 'limits.cpu': '5000m', 'limits.memory': '3448Mi'})
        self.assertEqual(quotas['reviewer-bounds'], {'pods': '2', 'configmaps': '4', 'requests.cpu': '1500m', 'requests.memory': '1724Mi', 'limits.cpu': '4500m', 'limits.memory': '2748Mi'})
        config = next(d['data'] for d in docs if d['kind'] == 'ConfigMap' and 'AI_SDLC_RUNTIME_MODE' in d.get('data', {}))
        self.assertEqual((config['AI_SDLC_SANDBOX_POD_OVERHEAD_CPU'], config['AI_SDLC_SANDBOX_POD_OVERHEAD_MEMORY']), ('250m', '350Mi'))
        # A project quota is extended too; without an overhead nothing changes.
        project = {'requests.cpu': '1500m', 'requests.memory': '3Gi', 'limits.cpu': '6', 'limits.memory': '6Gi'}
        result, docs = self.sandbox_docs({'project': {'verifierQuota': project}, 'sandbox': {'podOverhead': {'cpu': '100m', 'memory': ''}}})
        self.assertEqual(result.returncode, 0, result.stderr)
        hard = next(d['spec']['hard'] for d in docs if d['kind'] == 'ResourceQuota' and d['metadata']['name'] == 'verifier-bounds')
        self.assertEqual(hard, {'pods': '4', 'configmaps': '4', 'requests.cpu': '1900m', 'requests.memory': '3072Mi', 'limits.cpu': '6400m', 'limits.memory': '6144Mi'})
        result, docs = self.sandbox_docs()
        self.assertNotIn('SANDBOX_POD_OVERHEAD', result.stdout)
        hard = next(d['spec']['hard'] for d in docs if d['kind'] == 'ResourceQuota' and d['metadata']['name'] == 'executor-bounds')
        self.assertEqual(hard['requests.cpu'], '1')
        for values in ({'sandbox': {'podOverhead': {'cpu': '0.25'}}}, {'sandbox': {'podOverhead': {'memory': '1Gi'}}},
                       {'sandbox': {'podOverhead': {'cpu': '100m', 'other': '1'}}},
                       {'project': {'verifierQuota': dict(project, **{'requests.memory': '3G'})}, 'sandbox': {'podOverhead': {'memory': '10Mi'}}}):
            with self.subTest(values=values):
                self.assertNotEqual(self.sandbox_docs(values)[0].returncode, 0)

    def test_openshift_profile_binds_only_nonroot_scc_and_sets_platform(self):
        base = ['--set', 'runtime.mode=verification', '--set', 'verifier.digest=sha256:' + 'a' * 64]
        plain = [d for d in yaml.safe_load_all(self.render(*base).stdout) if d]
        self.assertFalse([d for d in plain if d['kind'] == 'RoleBinding' and d['metadata']['name'] == 'ai-sdlc-scc-nonroot-v2'])
        self.assertFalse([d for d in plain if d['kind'] == 'ConfigMap' and 'AI_SDLC_PLATFORM' in d.get('data', {})])
        result = self.render(*base, '--set', 'openshift.enabled=true')
        self.assertEqual(result.returncode, 0, result.stderr)
        docs = [d for d in yaml.safe_load_all(result.stdout) if d]
        bindings = {d['metadata']['namespace']: d for d in docs if d['kind'] == 'RoleBinding' and d['metadata']['name'] == 'ai-sdlc-scc-nonroot-v2'}
        self.assertEqual(set(bindings), {'ai-sdlc-runtime', 'ai-sdlc-executors', 'ai-sdlc-verifiers'})
        for namespace, binding in bindings.items():
            self.assertEqual(binding['roleRef'], {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'ClusterRole', 'name': 'system:openshift:scc:nonroot-v2'})
            if namespace == 'ai-sdlc-runtime':
                self.assertEqual(binding['subjects'], [{'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Group', 'name': 'system:serviceaccounts:ai-sdlc-runtime'}])
            else:
                self.assertEqual(binding['subjects'], [{'kind': 'ServiceAccount', 'name': 'default', 'namespace': namespace}])
        config = next(d['data'] for d in docs if d['kind'] == 'ConfigMap' and 'AI_SDLC_RUNTIME_MODE' in d.get('data', {}))
        self.assertEqual(config['AI_SDLC_PLATFORM'], 'openshift')
        self.assertNotEqual(self.render(*base, '--set', 'openshift.enabled=yes').returncode, 0)
        def dns_rules(documents):
            return {(d['metadata']['name'], json.dumps(r['to'], sort_keys=True), json.dumps(r['ports'], sort_keys=True))
                    for d in documents if d['kind'] == 'NetworkPolicy' for r in d['spec'].get('egress', [])
                    if any(p.get('port') in (53, 5353) for p in r.get('ports', []))}
        openshift_dns = dns_rules(docs)
        self.assertTrue(openshift_dns)
        for _, to, ports in openshift_dns:
            self.assertEqual(json.loads(to), [{'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': 'openshift-dns'}},
                                               'podSelector': {'matchLabels': {'dns.operator.openshift.io/daemonset-dns': 'default'}}}])
            self.assertEqual(json.loads(ports), [{'protocol': 'UDP', 'port': 5353}, {'protocol': 'TCP', 'port': 5353}])
        for _, to, _ in dns_rules(plain):
            self.assertIn('kube-system', to)



class ProjectManifestChartTests(unittest.TestCase):
    def test_project_manifest_is_rendered_only_when_set_and_immutable(self):
        tests = ChartTests()
        default = tests.render()
        self.assertEqual(default.returncode, 0, default.stderr)
        self.assertNotIn('name: ai-sdlc-project', default.stdout)
        manifest = '{"version":"ai-sdlc.project/v1"}'
        name = 'ai-sdlc-project-' + hashlib.sha256(manifest.encode()).hexdigest()[:12]
        with tempfile.NamedTemporaryFile('w', suffix='.yaml') as values:
            yaml.safe_dump({'project': {'manifestJSON': '{"version":"ai-sdlc.project/v1"}'}}, values)
            values.flush()
            rendered = tests.render('-f', values.name)
        self.assertEqual(rendered.returncode, 0, rendered.stderr)
        documents = [d for d in yaml.safe_load_all(rendered.stdout) if d and d.get('kind') == 'ConfigMap'
                     and d['metadata']['name'] == name]
        self.assertEqual(len(documents), 1)
        self.assertIs(documents[0]['immutable'], True)
        self.assertEqual(documents[0]['data'], {'manifest.json': '{"version":"ai-sdlc.project/v1"}'})
        pods = [d['spec']['template']['spec'] for d in yaml.safe_load_all(rendered.stdout)
                if d and d.get('kind') in ('Deployment', 'StatefulSet')]
        checked = 0
        for pod in pods:
            mounts = {m['name']: m for c in pod.get('containers', []) for m in c.get('volumeMounts', [])}
            if 'authority' not in mounts:
                continue
            checked += 1
            self.assertEqual(mounts['project']['mountPath'], '/etc/ai-sdlc/project')
            self.assertIs(mounts['project']['readOnly'], True)
            volumes = {v['name']: v for v in pod.get('volumes', [])}
            self.assertEqual(volumes['project']['configMap']['name'], name)
        self.assertGreater(checked, 0)
        self.assertNotIn('/etc/ai-sdlc/project', default.stdout)


    def test_repository_tests_get_exec_and_a_sized_quota_only_with_a_manifest(self):
        tests = ChartTests()
        base = ['--set', 'runtime.mode=verification', '--set', 'verifier.digest=sha256:' + 'a' * 64]
        def verifier_objects(rendered):
            docs = [d for d in yaml.safe_load_all(rendered.stdout) if d]
            role = next(d for d in docs if d['kind'] == 'Role' and d['metadata']['name'] == 'verification-controller')
            quota = next(d for d in docs if d['kind'] == 'ResourceQuota' and d['metadata']['name'] == 'verifier-bounds')
            return [r for rule in role['rules'] for r in rule['resources']], quota['spec']['hard']
        default = tests.render(*base)
        self.assertEqual(default.returncode, 0, default.stderr)
        resources, hard = verifier_objects(default)
        self.assertNotIn('pods/exec', resources)
        self.assertEqual(hard['requests.cpu'], '1')
        values = {'project': {'manifestJSON': '{"version":"ai-sdlc.project/v1"}', 'verifierQuota': {
            'requests.cpu': '1560m', 'requests.memory': '1632Mi', 'limits.cpu': '2100m', 'limits.memory': '1728Mi'}}}
        with tempfile.NamedTemporaryFile('w', suffix='.yaml') as file:
            yaml.safe_dump(values, file)
            file.flush()
            rendered = tests.render(*base, '-f', file.name)
        self.assertEqual(rendered.returncode, 0, rendered.stderr)
        resources, hard = verifier_objects(rendered)
        self.assertIn('pods/exec', resources)
        self.assertEqual((hard['requests.cpu'], hard['requests.memory'], hard['limits.cpu'], hard['limits.memory'], hard['pods']),
                         ('1560m', '1632Mi', '2100m', '1728Mi', '4'))
        values['project']['verifierQuota']['requests.cpu'] = '1; rm'
        with tempfile.NamedTemporaryFile('w', suffix='.yaml') as file:
            yaml.safe_dump(values, file)
            file.flush()
            self.assertNotEqual(tests.render(*base, '-f', file.name).returncode, 0)

    def test_executor_quota_grows_only_with_a_generation_toolchain(self):
        """The quota derives from manifestJSON itself, so an upgrade that keeps
        an older values file can never run a toolchain pod under the old quota."""
        tests = ChartTests()
        def manifest(generation):
            m = {'version': 'ai-sdlc.project/v1', 'repository': 'example/service'}
            if generation is not None:
                m['generation'] = generation
            return json.dumps(m, separators=(',', ':'))
        def executor_hard(values):
            rendered, docs = tests.sandbox_docs(values)
            self.assertEqual(rendered.returncode, 0, rendered.stderr)
            return next(d for d in docs if d['kind'] == 'ResourceQuota' and d['metadata']['name'] == 'executor-bounds')['spec']['hard']
        original = {'pods': '4', 'configmaps': '8', 'requests.cpu': '1', 'requests.memory': '1Gi', 'limits.cpu': '6', 'limits.memory': '4Gi'}
        self.assertEqual(executor_hard({}), original)
        self.assertEqual(executor_hard({'project': {'manifestJSON': manifest(None)}}), original)
        toolchain = {'toolchain': {'root': '/usr/local/go', 'path': ['bin']}}
        # executor.ToolchainExecutorQuota: four pods with 512Mi more worker memory each.
        self.assertEqual(executor_hard({'project': {'manifestJSON': manifest(toolchain)}}), dict(original, **{'limits.memory': '6Gi'}))
        for odd in ({}, {'toolchain': None}, {'toolchain': 'x'}):
            self.assertEqual(executor_hard({'project': {'manifestJSON': manifest(odd)}}), original, odd)
        # The RuntimeClass overhead (#769) composes: four pods' overhead is added on top.
        hard = executor_hard({'project': {'manifestJSON': manifest(toolchain)}, 'sandbox': {'podOverhead': {'cpu': '100m', 'memory': '64Mi'}}})
        self.assertEqual(hard, {'pods': '4', 'configmaps': '8', 'requests.cpu': '1400m', 'requests.memory': '1280Mi', 'limits.cpu': '6400m', 'limits.memory': '6400Mi'})
        self.assertNotIn('executorQuota', (ROOT / 'runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime/values.yaml').read_text())

    def test_publication_accepts_only_the_policy_bound_manifest_repository(self):
        tests = ChartTests()
        manifest = json.dumps({'version': 'ai-sdlc.project/v1', 'repository': 'example/service'}, separators=(',', ':'))
        digest = hashlib.sha256(manifest.encode()).hexdigest()
        def render(repository, bound):
            values = {'runtime': {'mode': 'verification'}, 'verifier': {'digest': 'sha256:' + 'a' * 64},
                      'review': {'enabled': True, 'configJSON': '{}'}, 'publication': {'enabled': True},
                      'project': {'manifestJSON': manifest},
                      'authority': {'policyJSON': json.dumps({'repository': repository, 'projectSHA256': bound})}}
            with tempfile.NamedTemporaryFile('w', suffix='.json') as file:
                json.dump(values, file)
                file.flush()
                return tests.render('-f', file.name)
        accepted = render('example/service', digest)
        self.assertEqual(accepted.returncode, 0, accepted.stderr)
        nameless = json.dumps({'version': 'ai-sdlc.project/v1'}, separators=(',', ':'))
        manifest_backup = manifest
        for repository, bound in (('example/other', digest), ('example/service', 'b' * 64), ('example/service', '')):
            refused = render(repository, bound)
            self.assertIn('supports only reviewed', refused.stderr, (repository, bound))
        # Neither the policy nor the manifest names a repository.
        values = {'runtime': {'mode': 'verification'}, 'verifier': {'digest': 'sha256:' + 'a' * 64},
                  'review': {'enabled': True, 'configJSON': '{}'}, 'publication': {'enabled': True},
                  'project': {'manifestJSON': nameless},
                  'authority': {'policyJSON': json.dumps({'projectSHA256': hashlib.sha256(nameless.encode()).hexdigest()})}}
        with tempfile.NamedTemporaryFile('w', suffix='.json') as file:
            json.dump(values, file)
            file.flush()
            self.assertIn('supports only reviewed', tests.render('-f', file.name).stderr)


if __name__ == '__main__': unittest.main()
