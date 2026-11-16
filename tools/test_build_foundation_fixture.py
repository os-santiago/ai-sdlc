import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import verify_build_foundation_node as fixture

class FixtureCleanupTests(unittest.TestCase):
    def test_execution_timeout_gets_independent_cleanup_budget_and_persists_failures(self):
        for cleanup_failure in (False,True):
            with self.subTest(cleanup_failure=cleanup_failure),tempfile.TemporaryDirectory() as directory:
                path=Path(directory)/'report.json';cid='c'*64;now=[0.0];nonce='a'*40;removed=[];budgets=[]
                def run(args,**kwargs):
                    if args[1]=='run':now[0]=1000;return subprocess.CompletedProcess(args,0,cid,'')
                    if args[1]=='exec':raise subprocess.TimeoutExpired(args,kwargs['timeout'])
                    if args[1]=='inspect':
                        budgets.append(kwargs['timeout'])
                        value=[{'Id':cid,'Config':{'Labels':{'ai-sdlc.foundation-node-fixture':nonce}}}]
                        return subprocess.CompletedProcess(args,0,json.dumps(value),'')
                    if args[1]=='rm':
                        removed.append(args);budgets.append(kwargs['timeout'])
                        if cleanup_failure:raise subprocess.TimeoutExpired(args,kwargs['timeout'])
                        return subprocess.CompletedProcess(args,0,'','')
                    raise AssertionError(args)
                argv=['fixture','--migrator-image','quay.io/os-santiago/ai-sdlc@sha256:'+'b'*64,'--output',str(path)]
                with patch.object(fixture.time,'monotonic',side_effect=lambda:now[0]),patch.object(fixture.secrets,'token_hex',return_value=nonce),patch.object(fixture.install,'run',side_effect=run),patch('sys.argv',argv):
                    with self.assertRaises(subprocess.TimeoutExpired):fixture.main()
                self.assertTrue(removed);self.assertTrue(all(t>=15 for t in budgets))
                report=json.loads(path.read_text());self.assertEqual(report['status'],'failed')
                self.assertEqual(report['cleanupComplete'],not cleanup_failure)
                if cleanup_failure:self.assertEqual(report['cleanupFailureType'],'TimeoutExpired')

if __name__=='__main__':unittest.main()
