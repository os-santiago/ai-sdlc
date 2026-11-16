"""Fault tests for administrative exclusion, not autonomous delivery evidence."""
import contextlib
import io
import base64
import copy
import json
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import uuid

import bootstrap_prepare as prep
import install


def resource(kind, name, **fields):
    return dict(kind=kind, metadata={'name':name,'uid':str(uuid.uuid5(uuid.NAMESPACE_DNS, kind+'/'+name))}, **fields)


class PreparationTests(unittest.TestCase):
    def test_misplaced_prepare_rejected_before_host_effects(self):
        with patch.object(install, "run") as run, contextlib.redirect_stderr(io.StringIO()), contextlib.ExitStack() as stack:
            with self.assertRaises(SystemExit) as result:
                install._main(["--cluster", "fixture", "prepare"], stack)
            self.assertEqual(result.exception.code, 2)
            run.assert_not_called()

    def setUp(self):
        self.roles = tuple(install.COMPONENT_NAMES.values())
        self.lock = {'image':{'repository':'quay.io/os-santiago/ai-sdlc','tag':'a'*40},
            'components':{k:{'digest':'sha256:'+'a'*64} for k in install.COMPONENT_NAMES},
            'migrator':{'digest':'sha256:'+'b'*64}}
        self.p = prep.Preparation(None, [], 'podman', 'fixture', Path('/unused'), self.lock)
        cluster = resource('Namespace','kube-system'); self.cluster_uid = cluster['metadata']['uid']
        policy = resource('ConfigMap','ai-sdlc-authority', data={'policy.json':json.dumps({
            'freeze':True,'repository':'os-santiago/saferus','environment':'ai-sdlc-production'})})
        policy['metadata']['annotations']={'ai-sdlc.io/cluster-uid':self.cluster_uid}
        self.sts = resource('StatefulSet','ai-sdlc-postgres')
        pvc = resource('PersistentVolumeClaim','data',spec={'volumeName':'data-volume'},status={'phase':'Bound'})
        pv = resource('PersistentVolume','data-volume',spec={'claimRef':{'uid':pvc['metadata']['uid']}})
        self.dbpod = resource('Pod','ai-sdlc-postgres-0',spec={'nodeName':'fixture-control-plane','volumes':[{'persistentVolumeClaim':{'claimName':'data'}}]},
            status={'podIP':'10.244.0.2','conditions':[{'type':'Ready','status':'True'}]})
        self.dbpod['metadata']['ownerReferences']=[dict(uid=self.sts['metadata']['uid'],controller=True)]
        secret = resource('Secret','ai-sdlc-postgres-app', data={'uri':base64.b64encode(b'postgresql://ai_sdlc:synthetic@ai-sdlc-postgres:5432/ai_sdlc?sslmode=disable').decode()})
        endpoint = resource('EndpointSlice','database', endpoints=[{'addresses':['10.244.0.2'],'conditions':{'ready':True},'targetRef':{'uid':self.dbpod['metadata']['uid']}}])
        endpoint['metadata']['labels']={'kubernetes.io/service-name':'ai-sdlc-postgres'}
        node = resource('Node','fixture-control-plane',status={'nodeInfo':{'bootID':'fixture-boot'}})
        self.objects = {('namespace','kube-system'):cluster,('configmap','ai-sdlc-authority'):policy,
            ('configmap','ai-sdlc-config'):resource('ConfigMap','ai-sdlc-config',data={'AI_SDLC_RUNTIME_MODE':'maintenance'}),
            ('node','fixture-control-plane'):node,('secret','ai-sdlc-postgres-app'):secret,
            ('service','ai-sdlc-postgres'):resource('Service','ai-sdlc-postgres'),('statefulset','ai-sdlc-postgres'):self.sts,
            ('pod','ai-sdlc-postgres-0'):self.dbpod,('pvc','data'):pvc,('pv','data-volume'):pv}
        self.lists = {'endpointslices':[endpoint],'namespaces':[],'deployments':[],'statefulsets':[self.sts],
            'cronjobs,daemonsets,hpa':[],'jobs':[],'replicasets':[],'pods':[self.dbpod]}
        self.p.templates = {}
        for role in self.roles:
            template = {'metadata':{'labels':{'component':role}},'spec':{'containers':[{
                'name':role,'image':'quay.io/os-santiago/ai-sdlc@sha256:'+'a'*64,
                'env':[{'name':'DATABASE_URL','valueFrom':{'secretKeyRef':{'name':'ai-sdlc-database-'+role,'key':'uri'}}}],
                'envFrom':[{'configMapRef':{'name':'ai-sdlc-config'}}]}]}}
            deployment = resource('Deployment','ai-sdlc-'+role,spec={'replicas':1,'template':copy.deepcopy(template)},status={'observedGeneration':1})
            self.p.templates[deployment['metadata']['name']] = template
            replica = resource('ReplicaSet','rs-'+role,spec={'replicas':1,'template':copy.deepcopy(template)})
            replica['metadata']['ownerReferences']=[dict(uid=deployment['metadata']['uid'],name=deployment['metadata']['name'],controller=True)]
            pod = resource('Pod','pod-'+role,spec=dict(copy.deepcopy(template['spec']),nodeName='fixture-control-plane'),status={'phase':'Running'})
            pod['metadata'].update(copy.deepcopy(template['metadata']))
            pod['metadata']['ownerReferences']=[dict(uid=replica['metadata']['uid'],controller=True)]
            self.lists['deployments'].append(deployment); self.lists['replicasets'].append(replica); self.lists['pods'].append(pod)
            password='synthetic_'+role.replace('-','_')+'x'*48
            secret=resource('Secret','ai-sdlc-database-'+role,data={k:base64.b64encode(v.encode()).decode()
                for k,v in {'password':password,'uri':install.component_uri(role,password)}.items()})
            secret['metadata']['annotations']={'ai-sdlc.io/cluster-uid':self.cluster_uid}
            self.objects[('secret',secret['metadata']['name'])]=secret
        self.p.get=lambda kind,name=None,namespace=None: copy.deepcopy(self.objects[(kind,name)] if name else {'items':self.lists[kind]})
        self.p.call=lambda args:SimpleNamespace(stdout=json.dumps([{'Id':'a'*64,'Config':{'Labels':{'io.x-k8s.kind.cluster':'fixture'}},'State':{'Status':'running'}}]))
        self.namespace = patch.object(prep.platform,'namespace',return_value={'namespaceUID':'runtime-uid'})
        self.namespace.start(); self.addCleanup(self.namespace.stop)

    def test_original_authority_and_paused_positive_controls(self):
        original,_=self.p.observe()
        self.p.expected=original
        with self.assertRaisesRegex(ValueError,'excluded'):
            self.p.observe(paused=True)
        for d in self.lists['deployments']:d['spec']['replicas']=0
        for r in self.lists['replicasets']:r['spec']['replicas']=0
        with self.assertRaisesRegex(ValueError,'terminated'):
            self.p.observe(paused=True)
        self.lists['pods']=[self.dbpod]
        self.assertEqual(self.p.observe(paused=True)[0],original)

    def test_runtime_overrides_and_foreign_custody_rejected_before_pause(self):
        for target in ('deployment','pod','replicaset','database'):
            with self.subTest(target=target):
                before=copy.deepcopy((self.objects,self.lists))
                if target=='database':
                    self.objects[('secret','ai-sdlc-database-worker')]['data']['uri']=base64.b64encode(b'postgresql://other:synthetic@other-database:5432/work').decode()
                else:
                    source = (self.lists['deployments'][0]['spec']['template'] if target=='deployment' else
                              self.lists['replicasets'][0]['spec']['template'] if target=='replicaset' else self.lists['pods'][1])
                    container=source['spec']['containers'][0]
                    container['env'].append({'name':'AI_SDLC_RUNTIME_MODE','value':'candidate'})
                    if target=='replicaset':
                        # A controller can still create an unauthorized Pod
                        # after an apparently empty instantaneous inventory.
                        self.lists['pods'].pop(1)
                with self.assertRaises(ValueError):self.p.observe()
                self.objects,self.lists=before

    def test_foreign_producers_and_running_job_rejected_before_pause(self):
        for kind,item in [('pods',resource('Pod','foreign',spec={},status={'phase':'Running'})),
                          ('replicasets',resource('ReplicaSet','foreign')),
                          ('cronjobs,daemonsets,hpa',resource('DaemonSet','foreign')),
                          ('jobs',resource('Job','incomplete',status={'succeeded':1,'active':1}))]:
            with self.subTest(kind=kind):
                self.lists[kind].append(item)
                with self.assertRaises(ValueError):self.p.observe()
                self.lists[kind].pop()

    def test_policy_or_role_secret_change_during_inspection_is_rejected(self):
        for target in ('policy','role'):
            with self.subTest(target=target):
                before=copy.deepcopy(self.objects)
                def inspect(*args,**kwargs):
                    if target=='policy':
                        self.objects[('configmap','ai-sdlc-authority')]['data']['policy.json']='{"freeze":false}'
                    else:
                        self.objects[('secret','ai-sdlc-database-worker')]['metadata']['uid']=str(uuid.uuid4())
                    return {'version':'ai-sdlc.empty-bootstrap-observation/v1','empty':True,'observedAt':'fixture','deliveryAuthorized':False}
                with patch.object(prep.build_runtime_install,'component_call',side_effect=inspect),self.assertRaises(ValueError):
                    self.p.inspect()
                self.objects=before

    def test_history_rejection_never_changes_observed_configuration(self):
        before=copy.deepcopy((self.objects,self.lists))
        with patch.object(prep.build_runtime_install,'component_call',side_effect=ValueError('history')),self.assertRaisesRegex(ValueError,'history'):
            self.p.inspect()
        self.assertEqual((self.objects,self.lists),before)


if __name__=='__main__':unittest.main()
