"""Original-Pod endpoint binding and bounded administrative tunnel cleanup."""
import json
import os
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import bootstrap_database_forward as forward


class Process:
    def __init__(self,output):
        read,write=os.pipe();os.write(write,output);os.close(write)
        self.stdout=os.fdopen(read,'rb',buffering=0);self.returncode=None;self.terminated=False
    def poll(self):return self.returncode
    def terminate(self):self.terminated=True;self.returncode=0
    def wait(self,timeout):return self.returncode
    def kill(self):self.returncode=-9


class ForwardTests(unittest.TestCase):
    def run_case(self,output,identities=('original','original','original'),action=None):
        process=Process(output);reads=iter(identities)
        def run(args,**kwargs):
            return SimpleNamespace(stdout=json.dumps({'metadata':{'uid':next(reads)},'status':{'conditions':[{'type':'Ready','status':'True'}]}}))
        with patch.object(forward.subprocess,'Popen',return_value=process) as popen,patch.object(forward.shutil,'which',return_value='/usr/bin/timeout'):
            try:
                with forward.original_database(run,['kubectl'],'postgresql://ai_sdlc:synthetic@10.244.0.2:5432/ai_sdlc?sslmode=disable','original') as (uri,port):
                    self.assertEqual(uri,'postgresql://ai_sdlc:synthetic@127.0.0.1:15432/ai_sdlc?sslmode=disable')
                    self.assertEqual(port,15432)
                    if action:action()
            finally:
                self.assertTrue(process.terminated);self.assertTrue(process.stdout.closed)
                command=popen.call_args.args[0]
                self.assertEqual(command[:3],['/usr/bin/timeout','--kill-after=5s','75s'])
                self.assertIn(':5432',command);self.assertNotIn('synthetic',' '.join(command))

    def test_original_endpoint_and_successful_cleanup(self):
        self.run_case(b'Forwarding from 127.0.0.1:15432 -> 5432\n')

    def test_replaced_pod_before_or_after_connection_cannot_be_accepted(self):
        for identities in [('original','replacement'),('original','original','replacement')]:
            with self.subTest(identities=identities),self.assertRaisesRegex(ValueError,'changed'):
                self.run_case(b'Forwarding from 127.0.0.1:15432 -> 5432\n',identities)

    def test_arbitrary_host_port_output_and_overflow_are_rejected(self):
        for output in (b'Forwarding from 0.0.0.0:15432 -> 5432\n',b'Forwarding from 127.0.0.1:15432 -> 443\n',b'x'*1025):
            with self.subTest(output=output[:50]),self.assertRaises(ValueError):self.run_case(output)

    def test_failed_component_observation_still_closes_tunnel(self):
        def fail():raise ValueError('component denied')
        with self.assertRaisesRegex(ValueError,'component denied'):
            self.run_case(b'Forwarding from 127.0.0.1:15432 -> 5432\n',action=fail)


if __name__=='__main__':unittest.main()
