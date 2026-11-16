"""Fault injection for cold custody and actual rendered registry boundaries."""
import base64
import copy
import json
import os
import io
from pathlib import Path
import subprocess
import socket
import tempfile
import threading
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import uuid
import yaml

import managed_registry_install as registry
import managed_registry_probe as probe
import supply_chain_install as supply


def custody():
    publisher={'username':'konflux','password':'a'*64}
    return {'version':registry.CUSTODY_VERSION,'publisher':publisher,
        'reader':{'username':'ai-sdlc-verifier','password':'b'*64},
        'htpasswd':'konflux:$2a$12$'+'a'*53+'\nai-sdlc-verifier:$2a$12$'+'b'*53+'\n',
        'dockerConfig':{'auths':{probe.HOST:dict(publisher,auth=base64.b64encode(('konflux:'+publisher['password']).encode()).decode())}}}


class CustodyTests(unittest.TestCase):
    def test_missing_retained_marker_is_not_recreated(self):
        request=unittest.mock.Mock(return_value=(404,{},b''))
        with self.assertRaisesRegex(ValueError,'refusing to recreate'):
            probe.check_matrix(request,'a'*48,require_existing=True)
        self.assertEqual(request.call_count,1)
        self.assertEqual(request.call_args.args[0],'GET')

    def test_slow_progress_response_has_absolute_deadline(self):
        client,server=socket.socketpair()
        def drip():
            try:
                for _ in range(100):server.sendall(b'x');time.sleep(.03)
            except OSError:pass
            finally:server.close()
        worker=threading.Thread(target=drip);worker.start()
        started=time.monotonic()
        try:
            with io.BufferedReader(probe.DeadlineReader(client,started+.15)) as response:
                with self.assertRaises(TimeoutError):response.read(100)
            self.assertLess(time.monotonic()-started,.5)
        finally:
            client.close();worker.join(timeout=1)

    def test_runtime_or_build_namespace_blocks_registry_mutation(self):
        with tempfile.TemporaryDirectory() as directory:
            item=registry.Installer(Path(directory),{'clusterUID':'cluster','objects':{}},[],{},None,'cluster')
            for namespace in ('ai-sdlc-runtime','ai-sdlc-builds'):
                item.read=lambda kind,name: {'metadata':{'name':name}} if name==namespace else None
                with self.subTest(namespace=namespace),self.assertRaisesRegex(ValueError,'cold-only'):
                    item.require_cold()
            item.read=lambda *args:None
            item.require_cold()

    def test_conversion_rejected_and_original_credentials_reused(self):
        with tempfile.TemporaryDirectory() as directory:
            state=Path(directory);lock={'image':{'repository':'quay.io/os-santiago/ai-sdlc'},'migrator':{'digest':'sha256:'+'c'*64}}
            with patch.object(registry.build_runtime_install,'component_call',return_value=custody()) as generate:
                with self.assertRaisesRegex(ValueError,'new installation'):
                    registry.prepare(state,'fixture','podman',lock,True)
                generate.assert_not_called()
                first=registry.prepare(state,'fixture','podman',lock,False)
                second=registry.prepare(state,'fixture','podman',lock,True)
                self.assertEqual(first,second);generate.assert_called_once()
                self.assertEqual((state/'managed-registry.json').stat().st_mode&0o777,0o600)
                changed=copy.deepcopy(lock);changed['migrator']['digest']='sha256:'+'d'*64
                with self.assertRaisesRegex(ValueError,'input changed'):
                    registry.prepare(state,'fixture','podman',changed,True)

    def test_lost_secret_create_ack_recovers_original_without_second_create(self):
        with tempfile.TemporaryDirectory() as directory:
            item=registry.Installer(Path(directory),{'clusterUID':'cluster','objects':{}},[],{},None,'cluster')
            item.fence=lambda:None
            objects={};effects=[]
            item.read=lambda kind,name,namespace=None:copy.deepcopy(objects.get(name))
            desired={'apiVersion':'v1','kind':'Secret','metadata':{'name':'private','namespace':'kind-registry'},
                'type':'Opaque','immutable':True,'data':{'key':'Zml4dHVyZQ=='}}
            def create(args,value):
                effects.append(args);objects['private']=copy.deepcopy(value)
                objects['private']['metadata']['uid']=str(uuid.uuid4())
                raise TimeoutError('fixture lost acknowledgement')
            item.call=create
            with self.assertRaises(TimeoutError):item.ensure(desired)
            recovered=item.ensure(desired)
            self.assertEqual(len(effects),1)
            self.assertEqual(recovered['metadata']['uid'],item.state['objects']['Secret/private']['uid'])
            objects['private']['metadata']['uid']=str(uuid.uuid4())
            with self.assertRaisesRegex(ValueError,'custody changed'):item.ensure(desired)
            del objects['private']
            with self.assertRaisesRegex(ValueError,'refusing recreation'):item.ensure(desired)
            self.assertEqual(len(effects),1)

    def test_rejected_registry_conversion_has_no_provider_effect(self):
        with patch.object(supply,'install_platform') as operation:
            with self.assertRaisesRegex(ValueError,'reviewed --images'):
                supply.main(['local','--managed-registry'])
            operation.assert_not_called()


class RegistryChartTests(unittest.TestCase):
    def test_api_empty_network_lists_do_not_hide_extra_egress(self):
        desired={'podSelector':{'matchLabels':{'run':'registry'}},'policyTypes':['Ingress','Egress'],'ingress':[],'egress':[]}
        admitted=copy.deepcopy(desired);admitted.pop('ingress');admitted.pop('egress')
        self.assertEqual(registry.network_spec(admitted),registry.network_spec(desired))
        admitted['egress']=[{}]
        self.assertNotEqual(registry.network_spec(admitted),registry.network_spec(desired))

    def render(self,extra=None):
        values={'clusterUID':'11111111-1111-1111-1111-111111111111','creationNonce':'a'*48,'credentialRevision':'b'*64}
        values.update(extra or {})
        helm=os.environ.get('AI_SDLC_TEST_HELM','helm')
        return subprocess.run([helm,'template',registry.RELEASE,str(registry.CHART),'--namespace',registry.RELEASE_NAMESPACE,
            '-f','-'],input=json.dumps(values),text=True,capture_output=True,timeout=20)

    def test_real_chart_has_native_readonly_identity_and_retained_storage(self):
        result=self.render();self.assertEqual(result.returncode,0,result.stderr)
        docs=[d for d in yaml.safe_load_all(result.stdout) if d]
        self.assertFalse(any(d['kind']=='Secret' for d in docs))
        by_kind={d['kind']:d for d in docs}
        policy=json.loads(by_kind['ConfigMap']['data']['config.json'])['http']['accessControl']['repositories']
        self.assertEqual(set(policy),{probe.REPO,'**'})
        self.assertEqual(policy[probe.REPO]['policies'],[
            {'users':['konflux'],'actions':['read','create','update']},
            {'users':['ai-sdlc-verifier'],'actions':['read']}])
        self.assertEqual(policy['**']['defaultPolicy'],[])
        self.assertEqual(by_kind['StorageClass']['reclaimPolicy'],'Retain')
        self.assertEqual(by_kind['PersistentVolumeClaim']['metadata']['annotations']['helm.sh/resource-policy'],'keep')
        pod=by_kind['Deployment']['spec']['template']['spec']
        self.assertFalse(pod['automountServiceAccountToken'])
        self.assertTrue(pod['securityContext']['runAsNonRoot'])
        self.assertTrue(pod['containers'][0]['securityContext']['readOnlyRootFilesystem'])
        self.assertEqual(by_kind['NetworkPolicy']['spec']['egress'],[])
        self.assertTrue(all(d['metadata']['annotations'].get('ai-sdlc.io/creation-nonce')=='a'*48 for d in docs))

    def test_chart_rejects_private_credentials_and_mutable_images(self):
        for extra in ({'password':'synthetic'},{'image':'zot:latest'},{'storageSize':'20Gi'},{'clusterUID':'replaced'}):
            with self.subTest(extra=extra):self.assertNotEqual(self.render(extra).returncode,0)

    def test_declared_trust_storage_and_identity_drift_rejected(self):
        result=self.render();self.assertEqual(result.returncode,0,result.stderr)
        docs={d['kind']:d for d in yaml.safe_load_all(result.stdout) if d}
        def changed(kind,mutate):
            desired=docs[kind];current=copy.deepcopy(desired)
            self.assertTrue(registry.stable_chart_content(desired,current))
            mutate(current)
            self.assertFalse(registry.stable_chart_content(desired,current))
        changed('Bundle',lambda x:x['spec']['sources'].append({'useDefaultCAs':False}))
        changed('Certificate',lambda x:x['spec']['issuerRef'].update(name='other'))
        changed('StorageClass',lambda x:x.update(reclaimPolicy='Delete'))
        changed('ServiceAccount',lambda x:x.update(automountServiceAccountToken=True))
        changed('Service',lambda x:x['spec']['ports'][0].update(targetPort=4444))
        changed('PersistentVolumeClaim',lambda x:x['spec'].update(storageClassName='other'))

    def test_chart_uid_replacement_rejected_before_reapply(self):
        with tempfile.TemporaryDirectory() as directory:
            item=registry.Installer(Path(directory),{'clusterUID':'cluster','objects':{},'creationNonce':'a'*48,
                'chartObjects':{'ConfigMap/zot-config':'original'}},[],{},None,'cluster')
            desired={'kind':'ConfigMap','metadata':{'name':'zot-config','namespace':'kind-registry'},'data':{'config.json':'reviewed'}}
            current=copy.deepcopy(desired)
            current['metadata'].update(uid='original',annotations={'ai-sdlc.io/creation-nonce':'a'*48,
                'meta.helm.sh/release-name':registry.RELEASE,'meta.helm.sh/release-namespace':registry.RELEASE_NAMESPACE})
            item.chart_objects=lambda:[desired]
            item.read=lambda *args:copy.deepcopy(current)
            item.check_chart_objects()
            current['metadata']['uid']='replacement'
            with self.assertRaisesRegex(ValueError,'ownership changed'):item.check_chart_objects()
            current['metadata']['uid']='original';current['data']['config.json']='permissive'
            with self.assertRaisesRegex(ValueError,'configuration changed'):item.check_chart_objects()
