from datetime import datetime,timezone,timedelta
import copy
import unittest
import subprocess
from unittest.mock import Mock,patch

from verify_registry_node_pull import event_proof,create_namespaced,frozen


class PullEvidenceTests(unittest.TestCase):
    def test_replaced_or_terminating_namespace_receives_no_credential(self):
        value={'kind':'Secret','metadata':{'namespace':'original'},'data':{'synthetic':'never-written'}}
        for meta in ({'name':'original','uid':'replacement','labels':{'ai-sdlc.pull-fixture':'nonce'}},
                     {'name':'original','uid':'uid','labels':{'ai-sdlc.pull-fixture':'wrong'}},
                     {'name':'original','uid':'uid','labels':{'ai-sdlc.pull-fixture':'nonce'},'deletionTimestamp':'now'}):
            post=Mock()
            with self.assertRaises(ValueError):create_namespaced(lambda *a:{'metadata':meta},post,value,'original','uid','nonce')
            post.assert_not_called()

    def test_freeze_reads_always_receive_deadline_limit(self):
        deadline=Mock()
        deadline.run.side_effect=subprocess.TimeoutExpired(['kubectl'],20)
        with self.assertRaises(subprocess.TimeoutExpired):frozen(deadline,['kubectl'])
        self.assertEqual(deadline.run.call_args.kwargs['limit'],20)

    def fixture(self):
        stamp=datetime.now(timezone.utc).isoformat()
        pod={'metadata':{'uid':'original-pod','name':'reader','creationTimestamp':stamp}}
        image='registry-service.kind-registry/ai-sdlc-fixtures/saferus@sha256:'+'a'*64
        event={'metadata':{'uid':'original-event'},'involvedObject':{'uid':'original-pod','name':'reader','kind':'Pod'},
            'source':{'component':'kubelet','host':'original-node'},'lastTimestamp':stamp,'reason':'Pulled',
            'message':'Successfully pulled image "'+image+'" in 20ms'}
        return pod,image,event

    def test_only_fresh_exact_pod_and_image_kubelet_pull_is_evidence(self):
        pod,image,event=self.fixture()
        proof=event_proof([event],pod,image,'original-node',True)
        self.assertEqual(proof['result'],'pulled')
        self.assertNotIn('message',proof)
        for path,value in ((('involvedObject','uid'),'replacement'),(('source','component'),'other'),
                           (('source','host'),'other'),(('message',),'Container image "'+image+'" already present on machine'),
                           (('message',),'Successfully pulled image "other"'),(('reason',),'Failed'),
                           (('lastTimestamp',),(datetime.now(timezone.utc)-timedelta(minutes=1)).isoformat()),
                           (('lastTimestamp',),(datetime.now(timezone.utc)+timedelta(minutes=1)).isoformat())):
            changed=copy.deepcopy(event);target=changed
            for key in path[:-1]:target=target[key]
            target[path[-1]]=value
            with self.subTest(path=path,value=value):self.assertIsNone(event_proof([changed],pod,image,'original-node',True))

    def test_execution_failure_is_never_pull_proof_and_negative_requires_auth_denial(self):
        pod,image,event=self.fixture()
        event['reason']='Failed';event['message']='CreateContainerError: no executable'
        self.assertIsNone(event_proof([event],pod,image,'original-node',True))
        self.assertIsNone(event_proof([event],pod,image,'original-node',False))
        event['message']='Failed to pull image "'+image+'": 401 Unauthorized'
        self.assertEqual(event_proof([event],pod,image,'original-node',False)['result'],'anonymous-denied')
        self.assertIsNone(event_proof([event],pod,image,'original-node',True))
        event['message']='Failed to pull image "'+image+'": authorization failed: no basic auth credentials'
        self.assertEqual(event_proof([event],pod,image,'original-node',False)['result'],'anonymous-denied')
        event['message']='Failed to pull image "'+image+'": connection refused'
        self.assertIsNone(event_proof([event],pod,image,'original-node',False))


if __name__=='__main__':unittest.main()
