import hashlib
import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

import build_foundation_node as node
import install

IMAGE='quay.io/os-santiago/ai-sdlc@sha256:'+'a'*64
CID='b'*64
NODE='c'*64

class NodeInitializerTests(unittest.TestCase):
    def exercise(self, fault=None):
        calls=[];label={};state={'storageIdentitySHA256':'d'*64,'nodeBootID':'11111111-1111-1111-1111-111111111111'}
        def fake(args,**kwargs):
            calls.append(args)
            output=''
            if args[1]=='create':
                pair=args[args.index('--label')+1];label[pair.split('=')[0]]=pair.split('=')[1]
                if fault=='lost-create':raise subprocess.TimeoutExpired(args,45)
                output=CID
            elif args[1]=='inspect':output=json.dumps([{'Id':CID,'Config':{'Labels':label}}])
            elif args[1]=='cp' and args[2]==CID+':/component':Path(args[3]).write_bytes(b'normal-image-binary')
            elif args[1]=='exec':
                if 'bash' in args:
                    if 'mountIntent' not in state:raise AssertionError('mount intent missing')
                    output=json.dumps({'filesystems':[{'id':123,'source':state['mountIntent']['source'],'target':state['mountIntent']['target'],'fstype':'tmpfs'}]})
                elif 'mktemp' in args:output='/tmp/ai-sdlc-foundation.ABCDEF123456'
                elif 'sha256sum' in args:output=hashlib.sha256(b'normal-image-binary').hexdigest()+' component'
                elif 'prepare-build-slot' in args:output='{"phase":"installed"}'
                elif 'rm' in args and fault=='node-cleanup':raise subprocess.CalledProcessError(1,args)
            elif args[1]=='rm' and fault=='extract-cleanup':raise subprocess.CalledProcessError(1,args)
            return subprocess.CompletedProcess(args,0,output,'')
        with patch.object(install.subprocess,'run',side_effect=fake):
            if fault is None:node.initialize(install.run,'podman',NODE,IMAGE,state,lambda:None)
            else:
                with self.assertRaises((subprocess.TimeoutExpired,subprocess.CalledProcessError,ValueError)):
                    node.initialize(install.run,'podman',NODE,IMAGE,state,lambda:None)
        self.assertIn(['podman','rm','-f','-v',CID],calls)
        return state
    def test_success_uses_normal_runner_and_cleans_original_extractor(self):
        state=self.exercise();self.assertEqual(state['mountIntent']['id'],'123')
        self.assertEqual(state['initializer']['image'],IMAGE)
    def test_lost_create_reply_recovers_only_nonce_for_cleanup(self):self.exercise('lost-create')
    def test_cleanup_failure_is_reported(self):self.exercise('extract-cleanup')
    def test_node_cleanup_failure_does_not_skip_extractor_cleanup(self):self.exercise('node-cleanup')

if __name__=='__main__':unittest.main()
