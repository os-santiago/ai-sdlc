"""Rendered opt-in release boundaries; no cluster/E2E claims."""
import copy
import hashlib
import json
import unittest
import yaml
from test_helm import ChartTests


class ReleaseChartTests(unittest.TestCase):
    def values(self):
        values=ChartTests().build_values()
        build=json.loads(values['build']['configJSON'])
        build['profile'].update(clusterUID='11111111-1111-1111-1111-111111111111',namespaceUID='builds',publisherImage='quay.io/os-santiago/ai-sdlc@sha256:'+'c'*64)
        values['build']['configJSON']=json.dumps(build)
        values['candidate']={'apiServerCIDRs':['10.96.0.1/32','172.18.0.2/32']}
        trust={'version':'ai-sdlc.saferus-provenance/v1','scope':'kind-test-production','repository':'os-santiago/saferus','environment':'ai-sdlc-production','clusterUID':'11111111-1111-1111-1111-111111111111','buildNamespaceUID':'builds','publisherImage':build['profile']['publisherImage'],'verifierImage':'quay.io/os-santiago/ai-sdlc@sha256:'+'a'*64,'registryCA':'fixture-ca','signerPublicKey':'fixture-key'}
        values['provenance']={'enabled':True,'trustJSON':json.dumps(trust),'registryCredentialSecret':'ai-sdlc-provenance-registry','credentialRevision':'d'*64}
        target={'version':'ai-sdlc.saferus-release-target/v1','scope':'kind-test-production','repository':'os-santiago/saferus','environment':'ai-sdlc-production','clusterUID':'11111111-1111-1111-1111-111111111111','namespace':'ai-sdlc-production','namespaceUID':'namespace','deployment':'saferus-console','deploymentUID':'deployment','container':'console','service':'saferus-console','serviceUID':'service','pullSecret':'ai-sdlc-release-reader','pullSecretUID':'pull','tlsSecretUID':'tls','tlsCA':'fixture-ca','tlsHostname':'saferus-console','deploymentSpecSHA256':'e'*64,'serviceSpecSHA256':'f'*64,'baselineRuntimeDigest':'sha256:'+'7'*64}
        values['release']={'enabled':True,'digest':'sha256:'+'9'*64,'targetJSON':json.dumps(target)}
        profile={'nodeName':'fixture-node','nodeUID':'99999999-1111-1111-1111-111111111111','version':'ai-sdlc.saferus-browser-profile/v1','clusterUID':target['clusterUID'],'image':'quay.io/os-santiago/ai-sdlc@sha256:'+'a'*64,'runtimeDigest':'sha256:'+'b'*64,'caSHA256':hashlib.sha256(target['tlsCA'].encode()).hexdigest(),'runtimeConfigurationSHA256':'c'*64,'baselineExpectation':'baseline-seconds','candidateExpectation':'readable-duration'}
        for index,key in enumerate(('namespaceUID','runtimeClassUID','caConfigMapUID','denyPolicyUID','egressPolicyUID','consoleIngressPolicyUID'),2):profile[key]=f'{index:08d}-1111-1111-1111-111111111111'
        values['release']['browserProfileJSON']=json.dumps(profile)
        return values

    def render(self,values,*args):
        result=ChartTests().render_build(values,*args)
        self.assertEqual(result.returncode,0,result.stderr)
        return [d for d in yaml.safe_load_all(result.stdout) if d]

    def test_release_has_exact_identity_rbac_and_private_mounts(self):
        docs=self.render(self.values())
        deploy=next(d for d in docs if d['kind']=='Deployment' and d['metadata']['name']=='ai-sdlc-deployer')
        pod=deploy['spec']['template']['spec'];container=pod['containers'][0]
        self.assertEqual(len(pod['containers']),1)
        self.assertEqual(pod['serviceAccountName'],'ai-sdlc-deployer')
        self.assertFalse(pod['automountServiceAccountToken'])
        self.assertEqual(container['image'],'quay.io/os-santiago/ai-sdlc@sha256:'+'9'*64)
        self.assertEqual(container['securityContext']['capabilities']['drop'],['ALL'])
        self.assertTrue(container['securityContext']['readOnlyRootFilesystem'])
        self.assertEqual({v['secret']['secretName'] for v in pod['volumes'] if 'secret' in v},{'ai-sdlc-identity-deployer'})
        self.assertEqual(next(e for e in container['env'] if e['name']=='DATABASE_URL')['valueFrom']['secretKeyRef']['name'],'ai-sdlc-database-deployer')
        roles=[d for d in docs if d['kind'] in ('Role','ClusterRole') and d['metadata']['name'] in ('ai-sdlc-deployer','ai-sdlc-deployer-identity')]
        writes=[]
        for role in roles:
            for rule in role['rules']:
                self.assertFalse(set(rule['verbs'])&{'*','create','delete','update','deletecollection'})
                if 'patch' in rule['verbs']:writes.append((role['metadata'].get('namespace'),rule['resources'],rule['resourceNames']))
        self.assertEqual(writes,[('ai-sdlc-production',['deployments'],['saferus-console'])])
        secrets=next(rule for role in roles for rule in role['rules'] if rule['resources']==['secrets'])
        self.assertEqual(set(secrets['resourceNames']),{'ai-sdlc-release-reader','saferus-console-tls'})
        self.assertEqual(secrets['verbs'],['get'])
        config=next(d['data'] for d in docs if d['kind']=='ConfigMap' and 'AI_SDLC_RELEASE' in d.get('data',{}))
        self.assertEqual(config['AI_SDLC_RELEASE'],'true')
        self.assertTrue(json.loads(config['AI_SDLC_RELAY_ROUTES'])['deploy-release'].endswith(':8090/internal/deploy-release'))
        routes=json.loads(config['AI_SDLC_RELAY_ROUTES'])
        self.assertEqual(len(routes),11)
        self.assertTrue(routes['record-deployment'].endswith('/internal/record-deployment'))
        self.assertTrue(routes['record-production'].endswith('/internal/record-production'))
        orchestrator=next(d for d in docs if d['kind']=='Deployment' and d['metadata']['name']=='ai-sdlc-orchestrator')
        pod=orchestrator['spec']['template']['spec']
        self.assertFalse(pod['automountServiceAccountToken'])
        mounts={v['mountPath'] for v in pod['containers'][0]['volumeMounts']}
        self.assertIn('/etc/ai-sdlc/outer-policy',mounts)
        self.assertNotIn('/etc/ai-sdlc/release',mounts)
        self.assertNotIn('/etc/ai-sdlc/provenance',mounts)
        for name in ('ai-sdlc-release-manager','ai-sdlc-provenance-verifier'):
            item=next(d for d in docs if d['kind']=='Deployment' and d['metadata']['name']==name)
            mounts={v['mountPath'] for v in item['spec']['template']['spec']['containers'][0]['volumeMounts']}
            self.assertTrue({'/etc/ai-sdlc/browser','/etc/ai-sdlc/release','/etc/ai-sdlc/provenance','/etc/ai-sdlc/build','/etc/ai-sdlc/outer-policy'}<=mounts)

    def test_release_network_policy_is_restricted_and_mandatory(self):
        for shared in ('true','false'):
            docs=self.render(self.values(),'--set','networkPolicy.enabled='+shared)
            policies=[d for d in docs if d['kind']=='NetworkPolicy']
            deployer=next(p for p in policies if p['metadata']['name']=='ai-sdlc-deployer')
            self.assertEqual(deployer['spec']['policyTypes'],['Ingress','Egress'])
            self.assertEqual(deployer['spec']['ingress'][0]['ports'],[{'protocol':'TCP','port':8090}])
            cidrs={peer['ipBlock']['cidr'] for rule in deployer['spec']['egress'] for peer in rule['to'] if 'ipBlock' in peer}
            self.assertEqual(cidrs,{'10.96.0.1/32','172.18.0.2/32'})
            if shared=='true':
                shared_policy=next(p for p in policies if p['metadata']['name']=='ai-sdlc-internal')
                self.assertIn({'key':'app.kubernetes.io/component','operator':'NotIn','values':['deployer']},shared_policy['spec']['podSelector']['matchExpressions'])
            console=next(p for p in policies if p['metadata']['name']=='ai-sdlc-deployer-console')
            self.assertEqual(console['metadata']['namespace'],'ai-sdlc-production')
            peer=console['spec']['ingress'][0]['from'][0]
            self.assertEqual(peer['namespaceSelector']['matchLabels']['kubernetes.io/metadata.name'],'ai-sdlc-runtime')
            self.assertEqual(peer['podSelector']['matchLabels']['app.kubernetes.io/component'],'deployer')
            self.assertEqual(console['spec']['ingress'][0]['ports'],[{'protocol':'TCP','port':8443}])

    def test_release_does_not_unpause_and_rejects_missing_prerequisites(self):
        v=self.values();v['runtime']['paused']=True
        for key in ('policyJSON',):
            p=json.loads(v['authority'][key]);p['freeze']=True;v['authority'][key]=json.dumps(p)
        p=json.loads(v['build']['outerPolicyJSON']);p['freeze']=True;v['build']['outerPolicyJSON']=json.dumps(p)
        installed=json.loads(v['build']['configJSON'])
        installed.update(inner=json.loads(v['authority']['policyJSON']),outer=p)
        v['build']['configJSON']=json.dumps(installed)
        docs=self.render(v);deployments=[d for d in docs if d['kind']=='Deployment']
        self.assertEqual(len(deployments),10)
        self.assertTrue(all(d['spec']['replicas']==0 for d in deployments))
        import bootstrap_prepare
        observer=object.__new__(bootstrap_prepare.Preparation)
        observer.use_configured_render(docs)
        changed=copy.deepcopy(docs)
        config=next(d for d in changed if d['kind']=='ConfigMap' and d['metadata']['name']=='ai-sdlc-config')
        config['data']['AI_SDLC_RELEASE']='false'
        with self.assertRaises(ValueError):observer.use_configured_render(changed)
        changed=[d for d in docs if not (d['kind']=='Deployment' and d['metadata']['name']=='ai-sdlc-deployer')]
        with self.assertRaises(ValueError):observer.use_configured_render(changed)
        for fault in ('provenance','pin','target','api','unknown'):
            v=self.values()
            if fault=='provenance':v['provenance']['enabled']=False
            elif fault=='pin':v['release']['digest']='latest'
            elif fault=='target':v['release']['targetJSON']='{}'
            elif fault=='api':v['candidate']['apiServerCIDRs']=[]
            else:v['release']['url']='https://untrusted.invalid'
            self.assertNotEqual(ChartTests().render_build(v).returncode,0)
