import copy
import json
from pathlib import Path
import tempfile
import unittest
from build_foundation_state import Resources, ANNOTATION, body

UID='11111111-1111-1111-1111-111111111111'

class FoundationStateTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.path=Path(self.tmp.name)/'state.json';self.state={'resources':{}}
        self.actual=None;self.posts=0;self.lost=False
        self.desired={'apiVersion':'v1','kind':'ConfigMap','metadata':{'name':'trust','namespace':'ai-sdlc-builds'},'immutable':True,'data':{'key':'value'}}
        self.resources=Resources(self.path,self.state,lambda obj:copy.deepcopy(self.actual),self.create)
    def create(self,obj):
        self.posts+=1
        # Intent must already exist durably before the API effect.
        saved=json.loads(self.path.read_text());self.assertEqual(len(saved['resources']),1)
        self.actual=copy.deepcopy(obj);self.actual['metadata']['uid']=UID
        if self.lost:raise TimeoutError('lost reply')
        return copy.deepcopy(self.actual)
    def test_reapply_and_lost_reply_keep_original_uid_and_one_create(self):
        self.lost=True
        with self.assertRaises(TimeoutError):self.resources.ensure(self.desired)
        self.lost=False
        self.assertEqual(self.resources.ensure(self.desired)['metadata']['uid'],UID)
        self.assertEqual(self.resources.ensure(self.desired)['metadata']['uid'],UID)
        self.assertEqual(self.posts,1)
        self.assertEqual(self.path.stat().st_mode & 0o777,0o600)
    def test_absence_replacement_or_drift_never_recreates(self):
        self.resources.ensure(self.desired);original=copy.deepcopy(self.actual)
        for fault in ('absent','uid','nonce','data','extra','terminating'):
            self.actual=copy.deepcopy(original)
            if fault=='absent':self.actual=None
            elif fault=='uid':self.actual['metadata']['uid']='22222222-2222-2222-2222-222222222222'
            elif fault=='nonce':self.actual['metadata']['annotations'][ANNOTATION]='other'
            elif fault=='data':self.actual['data']['key']='changed'
            elif fault=='extra':self.actual['binaryData']={'extra':'aA=='}
            else:self.actual['metadata']['deletionTimestamp']='now'
            with self.assertRaises(ValueError):self.resources.ensure(self.desired)
            self.assertEqual(self.posts,1)
    def test_existing_resource_is_never_adopted(self):
        self.actual=copy.deepcopy(self.desired);self.actual['metadata']['uid']=UID
        with self.assertRaisesRegex(ValueError,'unowned'):self.resources.ensure(self.desired)
        self.assertFalse(self.path.exists());self.assertEqual(self.posts,0)
    def test_lost_reply_absence_is_quarantined(self):
        self.lost=True
        with self.assertRaises(TimeoutError):self.resources.ensure(self.desired)
        self.actual=None
        with self.assertRaisesRegex(ValueError,'unresolved'):self.resources.ensure(self.desired)
        self.assertEqual(self.posts,1)
    def test_native_admission_defaulting_is_finite_and_drift_is_visible(self):
        desired={'apiVersion':'admissionregistration.k8s.io/v1','kind':'ValidatingAdmissionPolicy','metadata':{'name':'owned'},'spec':{'matchConstraints':{'resourceRules':[{'operations':['CREATE'],'resources':['pods']}]}}}
        actual=copy.deepcopy(desired);match=actual['spec']['matchConstraints']
        match.update(matchPolicy='Equivalent',namespaceSelector={},objectSelector={});match['resourceRules'][0]['scope']='*'
        self.assertEqual(body(desired),body(actual))
        match['namespaceSelector']={'matchLabels':{'unexpected':'scope'}}
        self.assertNotEqual(body(desired),body(actual))

    def test_changed_desired_state_cannot_redefine_existing_custody(self):
        self.resources.ensure(self.desired)
        changed=copy.deepcopy(self.desired);changed['data']['key']='new'
        with self.assertRaisesRegex(ValueError,'desired state changed'):self.resources.ensure(changed)
        self.assertEqual(self.posts,1)

if __name__=='__main__':unittest.main()
