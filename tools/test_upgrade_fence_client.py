"""Actual pipe/process failures plus ownership tests for the private transport."""
import json
import os
import subprocess
import sys
import time
import tempfile
from pathlib import Path
import unittest
from unittest.mock import Mock,patch

from upgrade_fence_client import Fence,LABEL,HistoryRejected

IMAGE='quay.io/os-santiago/ai-sdlc@sha256:'+'a'*64
URI='postgresql://ai_sdlc:synthetic@127.0.0.1:19001/ai_sdlc?sslmode=disable'
NONCE='01234567-89ab-cdef-0123-456789abcdef'

class FenceClientTests(unittest.TestCase):
    def fixture(self):
        return Fence('podman',IMAGE,URI,19001,[],expires_at=time.time()+60)

    def response(self,**changes):
        return dict({'version':'ai-sdlc.upgrade-fence/v1','state':'held','nonce':NONCE,
                     'expiresAt':int((time.time()+50)*1000),'workflowRowsWritten':False,'upgradeAuthorized':False},**changes)

    def read(self,fence,raw):
        reader,writer=os.pipe()
        with os.fdopen(reader,'rb',buffering=0) as pipe:
            os.write(writer,raw);os.close(writer)
            fence.process=Mock(stdout=pipe)
            return fence.read('held')

    def test_original_nonce_and_expiry_cannot_be_replaced(self):
        for fault in ('nonce','expiresAt'):
            with self.subTest(fault=fault):
                fence=self.fixture();value=self.response()
                self.read(fence,json.dumps(value).encode()+b'\n')
                value[fault]=('b'*8+NONCE[8:]) if fault=='nonce' else value[fault]+1
                with self.assertRaises(ValueError):self.read(fence,json.dumps(value).encode()+b'\n')

    def test_contained_review_protocol_is_explicit_and_cannot_accept_legacy_ack(self):
        fence=Fence('podman',IMAGE,URI,19001,[],expires_at=time.time()+60,contained_review=True)
        self.assertEqual(fence.operation,'hold-reviewer-upgrade-fence')
        with self.assertRaises(ValueError):self.read(fence,json.dumps(self.response()).encode()+b'\n')
        self.read(fence,json.dumps(self.response(version='ai-sdlc.reviewer-upgrade-fence/v1')).encode()+b'\n')
        with self.assertRaises(ValueError):self.read(self.fixture(),json.dumps(self.response(version='ai-sdlc.reviewer-upgrade-fence/v1')).encode()+b'\n')
        with self.assertRaises(ValueError):Fence('podman',IMAGE,URI,19001,[],expires_at=time.time()+60,contained_review='true')

    def test_browser_protocol_is_distinct_and_no_fallback(self):
        fence=Fence('podman',IMAGE,URI,19001,[],expires_at=time.time()+60,browser_deployment=True)
        self.assertEqual(fence.operation,'hold-browser-upgrade-fence')
        for version in ('ai-sdlc.upgrade-fence/v1','ai-sdlc.reviewer-upgrade-fence/v1'):
            with self.assertRaises(ValueError):self.read(fence,json.dumps(self.response(version=version)).encode()+b'\n')
        self.read(fence,json.dumps(self.response(version='ai-sdlc.browser-upgrade-fence/v1')).encode()+b'\n')
        for flags in ({'browser_deployment':'true'},{'browser_deployment':True,'contained_review':True}):
            with self.assertRaises(ValueError):Fence('podman',IMAGE,URI,19001,[],expires_at=time.time()+60,**flags)

    def test_only_exact_initial_compiled_history_rejection_has_specific_type(self):
        rejected={'version':'ai-sdlc.upgrade-fence/v1','state':'rejected','reason':'history',
                  'workflowRowsWritten':False,'upgradeAuthorized':False}
        with self.assertRaises(HistoryRejected):
            self.read(self.fixture(),json.dumps(rejected).encode()+b'\n')
        faults=[{**rejected,'reason':'connection'}, {**rejected,'extra':True},
                {**rejected,'version':'foreign'}, {**rejected,'upgradeAuthorized':True},
                {**rejected,'workflowRowsWritten':True}]
        for value in faults:
            with self.subTest(value=value):
                with self.assertRaises(ValueError) as caught:
                    self.read(self.fixture(),json.dumps(value).encode()+b'\n')
                self.assertNotIsInstance(caught.exception,HistoryRejected)
        for raw in (b'',b'{broken}\n'):
            with self.assertRaises(ValueError) as caught:self.read(self.fixture(),raw)
            self.assertNotIsInstance(caught.exception,HistoryRejected)
        fence=self.fixture();self.read(fence,json.dumps(self.response()).encode()+b'\n')
        with self.assertRaises(ValueError) as caught:self.read(fence,json.dumps(rejected).encode()+b'\n')
        self.assertNotIsInstance(caught.exception,HistoryRejected)

    def test_stopped_helper_is_retained_until_original_identity_cleanup(self):
        fence=self.fixture();fence.write=Mock();fence.read=Mock();fence.owned=Mock(return_value='a'*64)
        with patch('upgrade_fence_client.subprocess.Popen') as start,patch('upgrade_fence_client.os.set_blocking'):
            self.assertIs(fence.__enter__(),fence)
            args=start.call_args.args[0]
            self.assertNotIn('--rm',args)
            self.assertIn(LABEL+'='+fence.owner,args)
        fence.process=None
        fence.owned=Mock(side_effect=['a'*64,None])
        with patch('upgrade_fence_client.subprocess.run',return_value=Mock(returncode=0)) as run:
            fence.close()
            self.assertEqual(run.call_args.args[0],['podman','rm','-f','a'*64])

    def test_rejects_authority_malformed_duplicate_extra_and_oversized_frames(self):
        values=[self.response(upgradeAuthorized=True),self.response(workflowRowsWritten=True),
                self.response(nonce='-'*36),self.response(expiresAt=True),self.response(extra=True)]
        raws=[json.dumps(value).encode()+b'\n' for value in values]
        raw=json.dumps(self.response()).encode()
        raws += [raw[:-1]+b',"upgradeAuthorized":false}\n',raw+b'\n'+raw+b'\n',b'x'*4097,b'']
        for raw in raws:
            with self.subTest(size=len(raw)):
                with self.assertRaises((ValueError,json.JSONDecodeError)):self.read(self.fixture(),raw)

    def test_fence_loss_terminates_actual_owned_command(self):
        fence=self.fixture();fence.check=Mock();fence.live=Mock(side_effect=[None,ValueError('lost original backend')])
        actual=subprocess.Popen;children=[]
        def start(*args,**kwargs):
            child=actual(*args,**kwargs);children.append(child);return child
        with patch('upgrade_fence_client.subprocess.Popen',side_effect=start):
            with self.assertRaises(ValueError):fence.command([sys.executable,'-c','import time;time.sleep(60)'])
        self.assertEqual(len(children),1);self.assertIsNotNone(children[0].poll())

    def test_command_descendants_cannot_survive_fence_loss(self):
        fence=self.fixture();fence.check=Mock()
        with tempfile.TemporaryDirectory() as directory:
            pidfile=Path(directory)/'pid'
            def live():
                if pidfile.exists():raise ValueError('lost original backend')
            fence.live=live
            # Publish only a complete PID: cancellation must not race file creation.
            script="import os,subprocess,sys,time; from pathlib import Path; p=subprocess.Popen([sys.executable,'-c','import time;time.sleep(60)']);Path(sys.argv[1]+'.tmp').write_text(str(p.pid));os.replace(sys.argv[1]+'.tmp',sys.argv[1]);time.sleep(60)"
            with self.assertRaises(ValueError):fence.command([sys.executable,'-c',script,str(pidfile)])
            pid=int(pidfile.read_text())
            for _ in range(100):
                stat=Path('/proc')/str(pid)/'stat'
                try:
                    if stat.read_text().split()[2]=='Z':break
                except (FileNotFoundError, ProcessLookupError):
                    # The kernel can reap the killed child during this read.
                    break
                time.sleep(.01)
            else:self.fail('Owned command descendant survived cancellation')

    def test_leader_remains_unreaped_until_group_cleanup(self):
        fence=self.fixture();fence.check=Mock();fence.live=Mock()
        kill=os.killpg;observed=[]
        def reserved(pid,sig):
            status=os.waitid(os.P_PID,pid,os.WEXITED|os.WNOHANG|os.WNOWAIT)
            self.assertIsNotNone(status);observed.append(pid)
            kill(pid,sig)
        with patch('installer_process.os.killpg',side_effect=reserved):
            self.assertEqual(fence.command([sys.executable,'-c',"print('ok')"]),b'ok\n')
        self.assertEqual(len(observed),1)

    def test_missing_response_is_bounded_by_original_deadline(self):
        fence=self.fixture();reader,writer=os.pipe()
        with os.fdopen(reader,'rb',buffering=0) as pipe:
            try:
                fence.process=Mock(stdout=pipe);fence.process.poll.return_value=None
                fence.deadline=time.monotonic()+.05
                with self.assertRaises(ValueError):fence.read('held')
            finally:os.close(writer)

    def test_foreign_or_replacement_container_cannot_be_removed(self):
        for fault in ('owner','identity','unavailable'):
            with self.subTest(fault=fault):
                fence=self.fixture();fence.container_id='a'*64
                obj={'Id':'b'*64 if fault=='identity' else 'a'*64,'Config':{'Labels':{LABEL:'foreign' if fault=='owner' else fence.owner}}}
                responses=[Mock(returncode=1 if fault=='unavailable' else 0,stdout='a'*12),Mock(returncode=0,stdout=json.dumps([obj]))]
                with patch('upgrade_fence_client.subprocess.run',side_effect=responses) as run:
                    with self.assertRaises(ValueError):fence.close()
                    self.assertFalse(any('rm' in call.args[0] for call in run.call_args_list))

    def test_nonfinite_and_invalid_forwarded_authority_rejected(self):
        for expiry in (float('nan'),float('inf'),True,time.time()-1):
            with self.assertRaises(ValueError):Fence('podman',IMAGE,URI,19001,[],expires_at=expiry)
        with self.assertRaises(ValueError):Fence('podman',IMAGE,URI.replace('127.0.0.1','remote'),19001,[],expires_at=time.time()+60)

if __name__=='__main__':unittest.main()
