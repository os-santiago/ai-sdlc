import json
import unittest
from unittest.mock import Mock

import lifecycle_diagnostics as d

UID='11111111-1111-4111-8111-111111111111'


class DiagnosticsTests(unittest.TestCase):
    def pod(self):
        return {'metadata':{'name':'original','namespace':'fixture','uid':UID},
                'spec':{'command':['private-sentinel'],'env':[{'value':'secret-sentinel'}]},
                'status':{'phase':'Pending','message':'private-sentinel',
                    'conditions':[{'type':'PodScheduled','status':'False','reason':'Unschedulable','message':'secret-sentinel'}],
                    'containerStatuses':[{'name':'worker','ready':False,'restartCount':0,
                        'state':{'waiting':{'reason':'ContainerCreating','message':'private-sentinel'}}}]}}

    def test_finite_status_never_copies_messages_specs_or_unknown_reasons(self):
        pod=self.pod();result=d.summary(pod,'fixture','original')
        self.assertEqual(result['containers'][0]['reason'],'ContainerCreating')
        self.assertEqual(result['conditions'][0]['reason'],'Unschedulable')
        pod['status']['containerStatuses'][0]['state']['waiting']['reason']='secret-sentinel'
        pod['status']['conditions'][0]['reason']='private-sentinel'
        result=d.summary(pod,'fixture','original')
        self.assertNotIn('sentinel',json.dumps(result))
        self.assertNotIn('spec',result)
        self.assertEqual(result['containers'][0]['reason'],'other')

    def test_termination_and_restart_numbers_are_bounded(self):
        pod=self.pod();c=pod['status']['containerStatuses'][0]
        c['state']={'terminated':{'exitCode':137,'reason':'OOMKilled','message':'secret-sentinel'}}
        result=d.summary(pod,'fixture','original')
        self.assertEqual(result['containers'][0]['exitCode'],137)
        c['restartCount']=True;c['state']['terminated']['exitCode']=999
        result=d.summary(pod,'fixture','original')
        self.assertIsNone(result['containers'][0]['restartCount'])
        self.assertIsNone(result['containers'][0]['exitCode'])

    def test_foreign_pod_or_replaced_namespace_never_yields_observation(self):
        for field in ('name','namespace','uid'):
            pod=self.pod();pod['metadata'][field]='foreign'
            with self.assertRaises(ValueError):d.summary(pod,'fixture','original')
        original=json.dumps({'metadata':{'uid':UID}}).encode()
        foreign=json.dumps({'metadata':{'uid':'replaced'}}).encode()
        for responses in ([foreign],[original,json.dumps(self.pod()).encode(),foreign]):
            command=Mock(side_effect=responses)
            with self.assertRaises(ValueError):d.observe(command,'fixture',UID,'original')
            self.assertTrue(all(call.kwargs['timeout']==1 for call in command.call_args_list))

    def test_absent_pod_does_not_replace_last_known_status(self):
        command=Mock(side_effect=[json.dumps({'metadata':{'uid':UID}}).encode(),b''])
        self.assertIsNone(d.observe(command,'fixture',UID,'original'))


if __name__=='__main__':unittest.main()
