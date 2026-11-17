import copy
import hashlib
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock

from browser_fixture_client import Client,pod_custody,host_image_binding


class ClientCustody(unittest.TestCase):
    def test_host_storage_identity_has_one_explicit_oci_binding(self):
        graph={'referenceDigest':'sha256:'+'a'*64,'manifestDigest':'sha256:'+'b'*64,'configurationDigest':'sha256:'+'c'*64}
        index='application/vnd.oci.image.index.v1+json';manifest='application/vnd.oci.image.manifest.v1+json'
        legacy={'Id':graph['configurationDigest']}
        indexed={'Id':graph['referenceDigest'],'Descriptor':{'digest':graph['referenceDigest'],'mediaType':index}}
        single={'Id':graph['manifestDigest'],'Descriptor':{'digest':graph['manifestDigest'],'mediaType':manifest}}
        for image,kind in ((legacy,'configuration'),(indexed,'index'),(single,'manifest')):
            self.assertEqual(host_image_binding(image,graph)['hostIdentityKind'],kind)
        for image in ({'Id':graph['referenceDigest']},
                      {'Id':graph['configurationDigest'],'Descriptor':{'digest':graph['configurationDigest'],'mediaType':index}},
                      {'Id':graph['configurationDigest'],'Descriptor':{'digest':graph['configurationDigest'],'mediaType':manifest}},
                      {'Id':graph['referenceDigest'],'Descriptor':{'digest':graph['manifestDigest'],'mediaType':index}},
                      {'Id':graph['referenceDigest'],'Descriptor':{'digest':graph['referenceDigest'],'mediaType':'unknown'}},
                      {'Id':'unbound'}):
            with self.subTest(image=image),self.assertRaises(ValueError):host_image_binding(image,graph)

    def test_changed_binary_prevents_build(self):
        with tempfile.TemporaryDirectory() as folder:
            out=Path(folder);binary=out/'input';binary.write_bytes(b'changed')
            item=Client(SimpleNamespace(binary=binary),'node','cid','nonce',out,Mock(),Mock(),Mock(),Mock(),hashlib.sha256(b'original').hexdigest())
            item.node_guard=Mock()
            with self.assertRaises(ValueError):item.prepare()
            item.run.assert_not_called()

    def test_replaced_secret_or_pod_cannot_execute_or_publish_result(self):
        for changed in ('secret-initial-data','secret-initial-immutable','secret-initial-type','secret-before-create','pod-before-logs','pod-after-logs','secret-after-logs'):
            with self.subTest(changed=changed),tempfile.TemporaryDirectory() as folder:
                saved={};kinds=[];pod_reads=0;secret_reads=0
                def create(obj):
                    kinds.append(obj['kind']);value=copy.deepcopy(obj);value['metadata']['uid']='11111111-1111-4111-8111-'+('1' if obj['kind']=='Secret' else '2')*12
                    if obj['kind']=='Secret':
                        if changed=='secret-initial-data':value['data']={'private.json':'Zm9yZWlnbg=='}
                        if changed=='secret-initial-immutable':value['immutable']=False
                        if changed=='secret-initial-type':value['type']='foreign'
                    saved[obj['kind']]=value;return value
                def get(kind,name,namespace):
                    nonlocal pod_reads,secret_reads
                    value=copy.deepcopy(saved[kind.capitalize()])
                    if kind=='secret':
                        secret_reads+=1
                        if changed=='secret-before-create' and secret_reads==1 or changed=='secret-after-logs' and secret_reads==3:
                            value['metadata']['uid']='foreign'
                    if kind=='pod':
                        pod_reads+=1
                        if changed=='pod-before-logs' and pod_reads==2 or changed=='pod-after-logs' and pod_reads==3:
                            value['metadata']['uid']='foreign'
                        value['status']={'containerStatuses':[{'restartCount':0,'imageID':'localhost/test@sha256:'+'a'*64,
                            'state':{'terminated':{'exitCode':0,'message':json.dumps({'status':'passed'})}}}]}
                    return value
                out=Path(folder);run=Mock(return_value=b'public test log')
                item=Client(SimpleNamespace(),'node','cid','nonce',out,run,Mock(),create,get,'hash')
                item.node_guard=Mock();item.image='localhost/test@sha256:'+'a'*64
                with self.assertRaises(ValueError):item.execute(['kubectl'],'fixture','prefix','baseline',{'private':'fixture'})
                if changed=='secret-before-create' or changed.startswith('secret-initial-'):self.assertEqual(kinds,['Secret'])
                self.assertEqual(run.call_count,1 if changed in ('pod-after-logs','secret-after-logs') else 0)
                self.assertEqual(list(out.iterdir()),[])

    def test_changed_pod_authority_and_input_mount_rejected(self):
        desired={'metadata':{'name':'test','namespace':'fixture','labels':{'owner':'fixture'}},
            'spec':{'nodeName':'node','serviceAccountName':'original','containers':[{'name':'test','image':'fixed'}],
                'volumes':[{'name':'input','secret':{'secretName':'original'}}]}}
        original=copy.deepcopy(desired);original['metadata']['uid']='original'
        pod_custody(original,original,desired,'node')
        for changed in ('uid','node','service-account','image','secret','labels','sidecar','privilege'):
            current=copy.deepcopy(original)
            if changed=='uid':current['metadata']['uid']='foreign'
            elif changed=='node':current['spec']['nodeName']='foreign'
            elif changed=='service-account':current['spec']['serviceAccountName']='admin'
            elif changed=='image':current['spec']['containers'][0]['image']='foreign'
            elif changed=='secret':current['spec']['volumes'][0]['secret']['secretName']='foreign'
            elif changed=='labels':current['metadata']['labels']['owner']='foreign'
            elif changed=='sidecar':current['spec']['containers'].append({'name':'foreign','image':'foreign'})
            elif changed=='privilege':current['spec']['containers'][0]['securityContext']={'privileged':True}
            with self.subTest(changed=changed),self.assertRaises(ValueError):pod_custody(current,original,desired,'node')


if __name__=='__main__':unittest.main()
