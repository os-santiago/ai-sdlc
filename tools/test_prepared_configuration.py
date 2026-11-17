"""Prepared installation rejection controls; these are not workflow evidence."""
import base64
import copy
import json
import os
import tempfile
import time
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import uuid
import yaml

import bootstrap_prepare
import candidate_install
import install
import managed_registry_install
import prepared_configuration as prepared
import prepared_resources
import test_provenance_helm
import test_bootstrap_prepare


def frozen_documents():
    chart=test_provenance_helm.ProvenanceChartTests()
    values=chart.provenance_values()
    values['runtime']['paused']=True
    for section,key in (('authority','policyJSON'),('build','outerPolicyJSON')):
        policy=json.loads(values[section][key]);policy['freeze']=True;values[section][key]=json.dumps(policy)
    build=json.loads(values['build']['configJSON'])
    build.update(inner=json.loads(values['authority']['policyJSON']),outer=json.loads(values['build']['outerPolicyJSON']))
    values['build']['configJSON']=json.dumps(build)
    result=chart.render_build(values)
    if result.returncode:raise AssertionError(result.stderr)
    return [d for d in yaml.safe_load_all(result.stdout) if d]


class PreparedResourceTests(unittest.TestCase):
    def setUp(self):
        self.documents=frozen_documents()
        self.objects={}
        for identity,desired in prepared_resources.documents(self.documents).items():
            current=copy.deepcopy(desired);meta=current['metadata']
            meta['uid']=str(uuid.uuid5(uuid.NAMESPACE_DNS,identity))
            meta.setdefault('labels',{})['app.kubernetes.io/managed-by']='Helm'
            meta.setdefault('annotations',{}).update({'meta.helm.sh/release-name':'ai-sdlc','meta.helm.sh/release-namespace':'ai-sdlc-runtime'})
            self.objects[(desired['kind'],meta['name'],meta.get('namespace'))]=current
        self.api=SimpleNamespace(read=lambda kind,name,ns:self.objects.get((kind,name,ns)),
            inventory=lambda kinds:[obj for obj in self.objects.values() if obj['kind'] in kinds.split(',') and obj['metadata'].get('namespace')=='ai-sdlc-runtime'])
        self.description={'documents':self.documents,'externalRuntime':{}}

    def test_one_inventory_replaces_only_reads_inside_its_scope(self):
        calls=[];reads=[];original=self.api.inventory
        self.api.inventory=lambda kinds:(calls.append(kinds) or copy.deepcopy(original(kinds)))
        self.api.read=lambda kind,name,ns:(reads.append((kind,name,ns)) or self.objects.get((kind,name,ns)))
        first=prepared_resources.observe(self.api,self.description)
        self.assertEqual(len(calls),1)
        self.assertTrue(reads)
        self.assertFalse(any(kind in prepared_resources.RUNTIME_VERSIONS and ns=='ai-sdlc-runtime' for kind,name,ns in reads))
        for kind,name,ns in reads:
            self.assertIn(prepared_resources.key(self.objects[(kind,name,ns)]),first)
        # The next call must fetch a new inventory and detect replacement.
        identity=next(k for k in self.objects if k[0]=='ConfigMap' and k[2]=='ai-sdlc-runtime')
        self.objects[identity]['metadata']['uid']=str(uuid.uuid4())
        with self.assertRaises(ValueError):prepared_resources.observe(self.api,self.description,first)
        self.assertEqual(len(calls),2)

    def test_inventory_missing_resource_never_falls_back_to_get(self):
        original=self.api.inventory
        self.api.inventory=lambda kinds:original(kinds)[1:]
        def forbidden(*args):raise AssertionError('Missing LIST object mixed with later GET')
        self.api.read=forbidden
        with self.assertRaises(ValueError):prepared_resources.observe(self.api,self.description)

    def test_inventory_rejects_duplicate_malformed_and_wrong_scope(self):
        original=self.api.inventory
        values=copy.deepcopy(original(','.join(prepared_resources.RUNTIME_VERSIONS)))
        for fault in ('duplicate','version-duplicate','version','namespace','missing-namespace','missing-name','name-type','kind','uid','deleting','metadata','object','not-list'):
            with self.subTest(fault=fault):
                items=copy.deepcopy(values);item=items[0]
                if fault=='duplicate':items.append(copy.deepcopy(item))
                elif fault=='version-duplicate':
                    other=copy.deepcopy(item);other['apiVersion']='foreign/v9';items.append(other)
                elif fault=='version':item['apiVersion']='foreign/v9'
                elif fault=='namespace':item['metadata']['namespace']='foreign'
                elif fault=='missing-namespace':item['metadata'].pop('namespace')
                elif fault=='missing-name':item['metadata'].pop('name')
                elif fault=='name-type':item['metadata']['name']=[]
                elif fault=='kind':item['kind']='Secret'
                elif fault=='uid':item['metadata']['uid']='invalid'
                elif fault=='deleting':item['metadata']['deletionTimestamp']='2026-11-17T00:00:00Z'
                elif fault=='metadata':item['metadata']=[]
                elif fault=='object':items[0]=None
                elif fault=='not-list':items={}
                self.api.inventory=lambda kinds,items=items:items
                with self.assertRaises(ValueError):prepared_resources.observe(self.api,self.description)

    def test_listed_objects_keep_all_helm_identity_and_content_guards(self):
        baseline=prepared_resources.observe(self.api,self.description)
        identity=next(k for k in self.objects if k[0]=='ConfigMap' and k[2]=='ai-sdlc-runtime')
        original=copy.deepcopy(self.objects[identity])
        for fault in ('uid','owner','release','namespace','label','content','deleting'):
            with self.subTest(fault=fault):
                item=copy.deepcopy(original);self.objects[identity]=item
                if fault=='uid':item['metadata']['uid']=str(uuid.uuid4())
                elif fault=='owner':item['metadata']['ownerReferences']=[{'uid':str(uuid.uuid4())}]
                elif fault=='release':item['metadata']['annotations']['meta.helm.sh/release-name']='foreign'
                elif fault=='namespace':item['metadata']['annotations']['meta.helm.sh/release-namespace']='foreign'
                elif fault=='label':item['metadata']['labels']['app.kubernetes.io/managed-by']='foreign'
                elif fault=='content':item.setdefault('data',{})['unexpected']='value'
                elif fault=='deleting':item['metadata']['deletionTimestamp']='2026-11-17T00:00:00Z'
                with self.assertRaises(ValueError):prepared_resources.observe(self.api,self.description,baseline)
        self.objects[identity]=original
        self.assertEqual(prepared_resources.observe(self.api,self.description,baseline),baseline)

    def test_complete_render_then_missing_security_or_partial_helm_rejected(self):
        baseline=prepared_resources.observe(self.api,self.description)
        self.assertGreater(len(baseline),30)
        for kind in ('NetworkPolicy','ServiceAccount','RoleBinding','Role','ConfigMap','Service'):
            with self.subTest(kind=kind):
                identity=next(k for k in self.objects if k[0]==kind)
                original=self.objects.pop(identity)
                with self.assertRaises(ValueError):
                    prepared_resources.observe(self.api,self.description,baseline)
                self.objects[identity]=original
        self.assertEqual(prepared_resources.observe(self.api,self.description,baseline),baseline)

    def test_security_drift_and_replaced_uid_rejected(self):
        baseline=prepared_resources.observe(self.api,self.description)
        for kind in ('NetworkPolicy','ServiceAccount','RoleBinding','Role'):
            identity=next(k for k in self.objects if k[0]==kind);original=copy.deepcopy(self.objects[identity])
            with self.subTest(kind=kind):
                item=self.objects[identity]
                if kind=='NetworkPolicy':item['spec']['egress']=[{}]
                elif kind=='ServiceAccount':item['automountServiceAccountToken']=True
                elif kind=='RoleBinding':item['subjects'][0]['name']='foreign'
                else:item['rules'].append({'apiGroups':['*'],'resources':['*'],'verbs':['*']})
                with self.assertRaises(ValueError):prepared_resources.observe(self.api,self.description,baseline)
                self.objects[identity]=copy.deepcopy(original)
                self.objects[identity]['metadata']['uid']=str(uuid.uuid4())
                with self.assertRaises(ValueError):prepared_resources.observe(self.api,self.description,baseline)
                self.objects[identity]=original

    def test_known_api_defaults_preserve_meaning_and_extra_permissions_do_not(self):
        identity=next(k for k in self.objects if k[0]=='Service')
        item=self.objects[identity];item['spec'].update(type='ClusterIP',sessionAffinity='None',internalTrafficPolicy='Cluster',clusterIP='10.96.0.9')
        for port in item['spec']['ports']:port['protocol']='TCP'
        prepared_resources.observe(self.api,self.description)
        item['spec']['type']='NodePort'
        with self.assertRaises(ValueError):prepared_resources.observe(self.api,self.description)

    def test_foreign_security_object_and_changed_platform_injection_rejected(self):
        extra={'apiVersion':'v1','kind':'ConfigMap','metadata':{'name':'trusted-ca','namespace':'ai-sdlc-runtime','uid':str(uuid.uuid4())},'data':{'ca-bundle.crt':'synthetic'}}
        self.objects[('ConfigMap','trusted-ca','ai-sdlc-runtime')]=extra
        with self.assertRaises(ValueError):prepared_resources.observe(self.api,self.description)
        self.description['externalRuntime']=prepared_resources.platform_resources(self.api)
        prepared_resources.observe(self.api,self.description)
        extra['data']['ca-bundle.crt']='changed'
        with self.assertRaises(ValueError):prepared_resources.observe(self.api,self.description)
        extra['data']['ca-bundle.crt']='synthetic'
        extra=copy.deepcopy(next(obj for obj in self.objects.values() if obj['kind']=='RoleBinding'))
        extra['metadata'].update(name='foreign-binding',namespace='ai-sdlc-runtime')
        self.objects[('RoleBinding','foreign-binding','ai-sdlc-runtime')]=extra
        with self.assertRaises(ValueError):prepared_resources.observe(self.api,self.description)

    def test_complete_source_and_target_accepted_but_partial_prefix_is_not(self):
        target=copy.deepcopy(self.description)
        added=next(d for d in target['documents'] if d['kind']=='NetworkPolicy')
        changed=next(d for d in target['documents'] if d['kind']=='ConfigMap')
        source=copy.deepcopy(self.description)
        source['documents']=[d for d in source['documents'] if prepared_resources.key(d)!=prepared_resources.key(added)]
        changed['data']['transition-fixture']='target'
        added_key=(added['kind'],added['metadata']['name'],added['metadata'].get('namespace'))
        added_current=self.objects.pop(added_key)
        source['resourceUIDs']=prepared_resources.observe(self.api,source)
        target['resourceUIDs']={key:source['resourceUIDs'].get(key) for key in prepared_resources.documents(target['documents'])}
        foundation={'clusterUID':'cluster','namespaceUID':'namespace','nodeID':'node','nodeUID':'node-uid',
            'nodeBootID':'boot','databaseSecretSHA256':'same','resources':{},'roleSecrets':{},'deployments':{}}
        c=prepared.Configuration.__new__(prepared.Configuration)
        c.deadline=time.monotonic()+30;c.record={'source':source,'target':target};c.original={'original':foundation}
        c.read=self.api.read;c.inventory=self.api.inventory;c.check_secrets=lambda:None
        c.observer=lambda description:SimpleNamespace(observe=lambda paused:(foundation,None),inspect=lambda paused:foundation)
        c.guard();self.assertEqual(c.current,'source')
        self.objects[added_key]=added_current
        with self.assertRaisesRegex(ValueError,'neither'):c.guard()
        self.objects[(changed['kind'],changed['metadata']['name'],changed['metadata'].get('namespace'))]['data']=changed['data']
        c.guard();self.assertEqual(c.current,'target')

    def test_configured_observer_requires_both_freezes_and_all_paused(self):
        observer=bootstrap_prepare.Preparation(None,[],'podman','fixture',Path('/unused'),{})
        observer.use_configured_render(self.documents)
        self.assertEqual(len(observer.templates),9)
        for field in ('replicas','outer-freeze','inner-freeze'):
            docs=copy.deepcopy(self.documents)
            if field=='replicas':next(d for d in docs if d['kind']=='Deployment')['spec']['replicas']=1
            else:
                name='ai-sdlc-outer-policy' if field=='outer-freeze' else 'ai-sdlc-authority'
                d=next(d for d in docs if d['kind']=='ConfigMap' and d['metadata']['name']==name)
                policy=json.loads(d['data']['policy.json']);policy['freeze']=False;d['data']['policy.json']=json.dumps(policy)
            with self.assertRaises(ValueError):observer.use_configured_render(docs)


class ConfiguredProducerTests(unittest.TestCase):
    def test_real_render_matches_closed_paused_observer_with_separate_verifier_role(self):
        fixture=test_bootstrap_prepare.PreparationTests()
        fixture.setUp();self.addCleanup(fixture.doCleanups)
        docs=frozen_documents()
        for obj in docs:
            if obj['kind']=='ConfigMap':
                actual=test_bootstrap_prepare.resource('ConfigMap',obj['metadata']['name'],data=obj.get('data',{}))
                actual['metadata']['annotations']={'ai-sdlc.io/cluster-uid':fixture.cluster_uid}
                fixture.objects[('configmap',obj['metadata']['name'])]=actual
        fixture.lists['deployments']=[];fixture.lists['replicasets']=[];fixture.lists['pods']=[fixture.dbpod]
        for obj in docs:
            if obj['kind']=='Deployment':
                actual=test_bootstrap_prepare.resource('Deployment',obj['metadata']['name'],spec=obj['spec'],status={'observedGeneration':1})
                fixture.lists['deployments'].append(actual)
        for role in ('verifier','build-controller'):
            password='synthetic_'+role.replace('-','_')+'x'*48
            secret=test_bootstrap_prepare.resource('Secret','ai-sdlc-database-'+role,data={k:base64.b64encode(v.encode()).decode()
                for k,v in {'uri':install.component_uri(role,password),'password':password}.items()})
            secret['metadata']['annotations']={'ai-sdlc.io/cluster-uid':fixture.cluster_uid}
            fixture.objects[('secret',secret['metadata']['name'])]=secret
        fixture.p.use_configured_render(docs)
        observed,_=fixture.p.observe(paused=True)
        self.assertEqual(len(observed['deployments']),9)
        self.assertIn('verifier',observed['roleSecrets'])
        self.assertNotIn('provenance-verifier',observed['roleSecrets'])
        fixture.lists['deployments'][0]['spec']['replicas']=1
        with self.assertRaises(ValueError):fixture.p.observe(paused=True)


class PreparedCustodyTests(unittest.TestCase):
    def setUp(self):
        self.directory=tempfile.TemporaryDirectory();self.addCleanup(self.directory.cleanup)
        self.config=prepared.Configuration.__new__(prepared.Configuration)
        self.config.path=Path(self.directory.name)/'intent.json'
        self.config.record={'resources':{},'privateSecrets':{},'originalSecrets':{}}
        self.config.guard=lambda **kwargs:self.config.check_secrets()
        self.config.kubectl=['fixture-kubectl']
        self.objects={};self.posts=0
        self.config.read=lambda kind,name,namespace='ai-sdlc-runtime':copy.deepcopy(self.objects.get(name))
        self.config.secret_inventory=lambda:{name:{'uid':obj['metadata']['uid'],'sha256':bootstrap_prepare.digest({'data':obj['data'],'type':obj['type'],'immutable':obj.get('immutable',False)})} for name,obj in self.objects.items()}
        def create(argv,**kwargs):
            desired=json.loads(kwargs['input']);self.posts+=1
            # Lost response after successful POST: persisted nonce is the only
            # allowed recovery identity; never generate replacement material.
            desired['metadata']['uid']=str(uuid.uuid4());self.objects[desired['metadata']['name']]=desired
            raise OSError('synthetic lost reply')
        self.config.base_run=create
        self.desired={'apiVersion':'v1','kind':'Secret','metadata':{'name':'ai-sdlc-model-client','namespace':'ai-sdlc-runtime',
            'annotations':{'ai-sdlc.io/cluster-uid':'fixture'},'labels':{'app.kubernetes.io/part-of':'ai-sdlc'}},
            'type':'Opaque','data':{'token':base64.b64encode(b'synthetic-only-token').decode()}}

    def test_lost_post_reply_recovers_same_secret_without_second_create(self):
        with self.assertRaises(OSError):self.config.ensure_secret(self.desired)
        self.assertEqual(self.posts,1)
        persisted=json.loads(self.config.path.read_text())
        self.assertEqual(persisted['privateSecrets']['ai-sdlc-model-client'],self.desired)
        self.config.record=persisted
        recovered=self.config.ensure_secret(self.desired)
        self.assertEqual(self.posts,1)
        self.assertEqual(recovered,self.objects['ai-sdlc-model-client'])
        self.config.check_secrets()
        self.assertEqual(self.config.path.stat().st_mode&0o777,0o600)

    def test_crash_before_post_does_not_restore_first_create_permission(self):
        self.config.record['privateSecrets']['ai-sdlc-model-client']=self.desired
        with self.assertRaises(ValueError):self.config.ensure_secret(self.desired)
        self.assertEqual(self.posts,0)

    def test_lost_reply_is_resolved_even_when_credential_helper_reuses_existing(self):
        with self.assertRaises(OSError):self.config.ensure_secret(self.desired)
        self.assertIsNone(next(iter(self.config.record['resources'].values()))['uid'])
        self.config.resolve_secret_intents()
        self.assertEqual(next(iter(self.config.record['resources'].values()))['uid'],self.objects['ai-sdlc-model-client']['metadata']['uid'])
        self.assertEqual(self.posts,1)

    def test_changed_private_material_or_absent_original_cannot_be_adopted(self):
        with self.assertRaises(OSError):self.config.ensure_secret(self.desired)
        self.config.ensure_secret(self.desired)
        original=copy.deepcopy(self.objects)
        for fault in ('data','annotation','uid','absent'):
            self.objects=copy.deepcopy(original)
            item=self.objects['ai-sdlc-model-client']
            if fault=='absent':self.objects.clear()
            elif fault=='data':item['data']['token']=base64.b64encode(b'changed').decode()
            elif fault=='annotation':item['metadata']['annotations']['ai-sdlc.io/cluster-uid']='other'
            else:item['metadata']['uid']=str(uuid.uuid4())
            with self.subTest(fault=fault),self.assertRaises(ValueError):self.config.check_secrets()

    def test_configured_reapply_checks_private_custody_before_new_baseline(self):
        self.config.record.update(phase='configured-paused',target={'sentinel':'must-not-read'})
        self.config.check_secrets=lambda:(_ for _ in ()).throw(ValueError('changed original secret'))
        with self.assertRaisesRegex(ValueError,'changed original secret'):self.config.begin()

    def test_controller_revision_derivation_preserves_original_physical_profile(self):
        state=Path(self.directory.name)
        original={'version':'ai-sdlc.build-desired/v1','revision':'a'*40,'controllerDigest':'sha256:'+'a'*64,
            'profile':{'publisherImage':'quay.io/os-santiago/ai-sdlc@sha256:'+'b'*64,'nodeBootID':'original'},
            'slots':[{'pvcUID':'original'}],'issueMaxAgeSeconds':60}
        (state/'original.json').write_text(json.dumps(original))
        foundation={'version':'ai-sdlc.build-foundation/v1','status':'prepared','bundleSHA256':bootstrap_prepare.digest(original),
            'publisherImage':original['profile']['publisherImage']}
        (state/'build-foundation.json').write_text(json.dumps(foundation))
        selected={'version':'ai-sdlc.build-images/v1','revision':'c'*40,'publisherImage':'quay.io/os-santiago/ai-sdlc@sha256:'+'c'*64,'controllerDigest':'sha256:'+'d'*64}
        (state/'selected.json').write_text(json.dumps(selected))
        target,_=prepared.derive_bundle(state/'original.json',{'image':{'tag':'c'*40}},state/'selected.json',state)
        self.assertEqual(target['profile'],original['profile']);self.assertEqual(target['slots'],original['slots'])
        self.assertEqual(target['controllerDigest'],selected['controllerDigest'])
        original['slots'][0]['pvcUID']='replacement';(state/'original.json').write_text(json.dumps(original))
        with self.assertRaises(ValueError):prepared.derive_bundle(state/'original.json',{'image':{'tag':'c'*40}},state/'selected.json',state)

    def test_registry_source_observer_never_writes_or_enters_mutating_path(self):
        state=Path(self.directory.name)
        record={'clusterUID':'original'}
        observer=managed_registry_install.Installer(state,record,[],{},None,'original',read_only=True)
        self.assertFalse((state/'managed-registry.json').exists())
        with self.assertRaises(ValueError):observer.save(record)
        with self.assertRaises(ValueError):observer.require_cold()
        with self.assertRaises(ValueError):observer.install()
        self.assertFalse((state/'managed-registry.json').exists())

    def test_incomplete_command_rejected_before_tools_or_effects(self):
        for args in (['local','--configure-prepared'],['local','--build-images','/unused'],['local','--provenance-trust','/unused']):
            with patch.object(install,'tools') as tools,patch.object(install,'run') as run:
                with self.assertRaises(ValueError):install.main(args)
                tools.assert_not_called();run.assert_not_called()

    def test_prepared_entrypoint_defers_render_until_native_policy_is_known(self):
        selected={'merge':{'enabled':True}}
        args=['local','--platform','konflux','--cluster','prepared-fixture','--provider','podman',
            '--repository','os-santiago/saferus','--environment','ai-sdlc-production','--profile','verification',
            '--independent-review','--publication','--merge','--github-auth','gh-cli','--configure-prepared',
            '--build-bundle','/reviewed/bundle.json','--build-images','/reviewed/build-images.json',
            '--provenance-trust','/reviewed/trust.json']
        with tempfile.TemporaryDirectory() as directory,patch.dict(os.environ,{'XDG_STATE_HOME':directory}),\
                patch.object(install,'tools',return_value={'helm':'helm'}),\
                patch.object(candidate_install,'desired_values',return_value=selected),\
                patch.object(install.konflux_runtime_install,'lock_state'),\
                patch.object(install.konflux_runtime_install,'select_provider',return_value='podman'),\
                patch.object(install,'run',side_effect=AssertionError('Partial configuration must not be rendered')),\
                patch.object(prepared,'install_prepared',return_value=0) as transition:
            self.assertEqual(install.main(args),0)
        self.assertEqual(transition.call_args.args[-1],selected)
        self.assertNotIn('configJSON',selected['merge'])


if __name__=='__main__':unittest.main()
