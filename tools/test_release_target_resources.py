import copy
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock
import uuid

from release_target_resources import Patch,patch_request,verify_after

class PatchTests(unittest.TestCase):
    def fixture(self):
        return {'apiVersion':'apps/v1','kind':'Deployment','metadata':{'name':'saferus-console','namespace':'ai-sdlc-production','uid':str(uuid.uuid4()),'resourceVersion':'100','generation':1,'annotations':{'saferus.io/creation-nonce':'original'}},'spec':{'replicas':1,'template':{'spec':{'containers':[{'name':'console','image':'localhost/saferus-console@sha256:'+'a'*64}]}}}}
    def acknowledged(self,before,nonce):
        _,spec,annotations=patch_request(before,nonce);after=copy.deepcopy(before)
        after['spec']=spec;after['metadata'].update(generation=2,resourceVersion='200',annotations=annotations)
        return after
    def test_conditional_patch_binds_full_original_and_preserves_image(self):
        before=self.fixture();request,spec,_=patch_request(before,'a'*48)
        self.assertEqual([r['path'] for r in request[:4]],['/metadata/uid','/metadata/resourceVersion','/metadata/generation','/spec'])
        self.assertEqual(spec['template']['spec']['containers'],before['spec']['template']['spec']['containers'])
        for field,value in [('uid',str(uuid.uuid4())),('generation',3),('annotations',{})]:
            after=self.acknowledged(before,'a'*48);after['metadata'][field]=value
            with self.assertRaises(ValueError):verify_after(before,after,'a'*48)
        after=self.acknowledged(before,'a'*48);after['metadata']['annotations']['deployment.kubernetes.io/revision']='2';verify_after(before,after,'a'*48)
        after=self.acknowledged(before,'a'*48);after['spec']['replicas']=2
        with self.assertRaises(ValueError):verify_after(before,after,'a'*48)
    def test_lost_ack_resolves_only_same_original_nonce_without_resend(self):
        before=self.fixture();record={};live=[before];guard=Mock()
        with tempfile.TemporaryDirectory() as tmp:
            path=Path(tmp)/'record'
            def send(request):
                self.assertTrue(path.exists());live[0]=self.acknowledged(before,record['deploymentPatch']['nonce']);raise TimeoutError('lost ACK')
            patch=Patch(path,record,guard,lambda:live[0],Mock(side_effect=send))
            with self.assertRaises(TimeoutError):patch.apply(before)
            result=patch.apply(before);self.assertEqual(result,live[0]);patch.send.assert_called_once()
            patch.apply(before);patch.send.assert_called_once()
            live[0]=before
            with self.assertRaises(ValueError):patch.apply(before)
            patch.send.assert_called_once()
    def test_observed_same_image_without_original_nonce_is_not_adopted(self):
        before=self.fixture();record={'deploymentPatch':{'before':before,'nonce':'a'*48,'response':None}}
        with tempfile.TemporaryDirectory() as tmp:
            patch=Patch(Path(tmp)/'record',record,Mock(),lambda:self.acknowledged(before,'b'*48),Mock())
            with self.assertRaises(ValueError):patch.apply(before)
            patch.send.assert_not_called()
