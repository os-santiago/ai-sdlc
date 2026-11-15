import contextlib
import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import install
import supply_chain_install as supply


class SupplyChainTests(unittest.TestCase):
    def test_stale_generations_and_old_replicas_are_not_ready(self):
        for mode in ('current','cr-old','deployment-old','old-replicas','not-ready','deleting','missing','scaled-down'):
            cr={'metadata':{'uid':'fixture','generation':2},'status':{'conditions':[{'type':'Ready','status':'True','observedGeneration':2}]}}
            deployment={'metadata':{'namespace':'fixture','name':'fixture','generation':2},'spec':{'replicas':1},
                        'status':{'observedGeneration':2,'updatedReplicas':1,'readyReplicas':1,'availableReplicas':1}}
            if mode=='cr-old':cr['status']['conditions'][0]['observedGeneration']=1
            if mode=='deployment-old':deployment['status']['observedGeneration']=1
            if mode=='old-replicas':deployment['status']['updatedReplicas']=0
            if mode=='not-ready':deployment['status']['readyReplicas']=0
            if mode=='deleting':cr['metadata']['deletionTimestamp']='2026-11-15T00:00:00Z'
            if mode=='scaled-down':deployment['spec']['replicas']=0
            values={'konflux':cr,'deployments':{'items':[deployment]},'pods':{'items':[]}}
            if mode=='missing':values['deployments']['items']=[]
            class Reader:
                def run(self,args,**kwargs):return SimpleNamespace(stdout=json.dumps(values[args[1]]))
            with self.subTest(mode=mode):
                self.assertEqual(supply.observe([],Reader(),'uid','digest',['fixture/fixture'])['status'],'ready' if mode=='current' else 'not-ready')

    def test_recorded_provider_is_preserved_and_change_does_not_execute(self):
        with tempfile.TemporaryDirectory() as directory:
            marker=Path(directory)/'marker.json'
            original=json.dumps({'version':1,'cluster':'fixture','provider':'podman','clusterUID':'a'*8+'-'+'b'*4+'-'+'c'*4+'-'+'d'*4+'-'+'e'*12,'scope':'isolated-konflux'})
            marker.write_text(original)
            with patch.object(supply.shutil,'which',return_value='/installed'),patch.object(supply.subprocess,'run') as run:
                self.assertEqual(supply.select_provider('auto',marker,'fixture'),'podman')
                with self.assertRaisesRegex(ValueError,'provider differs'):
                    supply.select_provider('docker',marker,'fixture')
                run.assert_not_called()
            self.assertEqual(marker.read_text(),original)

    def test_slow_progress_download_has_a_real_wall_clock_deadline(self):
        class SlowHandler(BaseHTTPRequestHandler):
            def log_message(self,*args):pass
            def do_GET(self):
                self.send_response(200);self.end_headers()
                try:
                    for _ in range(30):
                        self.wfile.write(b'x');self.wfile.flush();time.sleep(.1)
                except (BrokenPipeError,ConnectionResetError):pass
        server=ThreadingHTTPServer(('127.0.0.1',0),SlowHandler)
        thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
        try:
            with tempfile.TemporaryDirectory() as directory:
                asset={'url':'http://127.0.0.1:'+str(server.server_port),'sha256':hashlib.sha256(b'x'*30).hexdigest()}
                started=time.monotonic()
                with self.assertRaisesRegex(ValueError,'wall-clock deadline'):
                    supply.verified_asset(asset,Path(directory),supply.Deadline(.3),64)
                self.assertLess(time.monotonic()-started,1.5)
                self.assertEqual(list(Path(directory).iterdir()),[])
        finally:
            server.shutdown();server.server_close();thread.join(timeout=1)

    def test_slow_tools_bootstrap_is_killed_within_wall_clock_budget(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);(root/'tools').mkdir();(root/'tools/install.py').write_text('import time\ntime.sleep(10)\n')
            started=time.monotonic()
            with patch.object(supply,'ROOT',root),self.assertRaisesRegex(ValueError,'wall-clock deadline'):
                supply.bootstrap_tools(supply.Deadline(.2))
            self.assertLess(time.monotonic()-started,1.5)

    def test_api_failure_cannot_disable_an_existing_helm_resource(self):
        for fail_at in (1,2):
            calls = 0
            class Reader:
                def run(self,args,**kwargs):
                    nonlocal calls
                    calls+=1
                    if calls==fail_at:
                        raise subprocess.CalledProcessError(1,args)
                    return SimpleNamespace(stdout='{}')
            with self.subTest(fail_at=fail_at),self.assertRaises(subprocess.CalledProcessError):
                supply.existing_konflux(['fixture-kubectl'],Reader())

    def test_existing_resource_requires_helm_ownership(self):
        for owned in (True,False):
            annotations = {'meta.helm.sh/release-name':supply.RELEASE,
                           'meta.helm.sh/release-namespace':supply.RELEASE_NAMESPACE} if owned else {}
            responses = iter(['{}',json.dumps({'metadata':{'annotations':annotations}})])
            class Reader:
                def run(self,*args,**kwargs):
                    return SimpleNamespace(stdout=next(responses))
            with self.subTest(owned=owned):
                if owned:
                    self.assertTrue(supply.existing_konflux([],Reader()))
                else:
                    with self.assertRaisesRegex(ValueError,'not owned'):
                        supply.existing_konflux([],Reader())

    def test_single_entrypoint_plan_declares_isolation_without_network(self):
        output = io.StringIO()
        with patch.object(supply.urllib.request,'urlopen',side_effect=AssertionError('plan accessed network')), \
             patch.object(supply.subprocess,'run',side_effect=AssertionError('plan executed a process')), \
             contextlib.redirect_stdout(output):
            self.assertEqual(install.main(['supply-chain','plan']),0)
        plan = json.loads(output.getvalue())
        self.assertEqual(plan['cluster'],'ai-sdlc-konflux')
        self.assertFalse(plan['liveWorkflowAdvanced'])
        self.assertEqual(plan['hostPortMappings'],[])
        self.assertEqual(plan['defaultUsers'],[])
        self.assertRegex(plan['operatorImage'],r'@sha256:[a-f0-9]{64}$')

    def test_tampered_download_is_never_cached_or_accepted(self):
        expected = b'reviewed manifest'
        asset = {'url':'https://example.invalid/fixture','sha256':hashlib.sha256(expected).hexdigest()}
        for raw in (b'tampered manifest', b'x'*100):
            deadline=supply.Deadline(30)
            def download(args,**kwargs):Path(args[-2]).write_bytes(raw)
            with self.subTest(raw=raw[:10]),tempfile.TemporaryDirectory() as directory, \
                 patch.object(deadline,'run',side_effect=download):
                cache = Path(directory)
                with self.assertRaisesRegex(ValueError,'size/digest'):
                    supply.verified_asset(asset,cache,deadline,32)
                self.assertEqual(list(cache.iterdir()),[])

    def test_verified_cache_is_rechecked_and_symlinks_are_rejected(self):
        raw = b'verified fixture'
        asset = {'url':'https://example.invalid/fixture','sha256':hashlib.sha256(raw).hexdigest()}
        with tempfile.TemporaryDirectory() as directory:
            cache = Path(directory)
            target = cache/asset['sha256']
            target.write_bytes(raw)
            deadline=supply.Deadline(30)
            with patch.object(deadline,'run',side_effect=AssertionError('verified cache downloaded')):
                self.assertEqual(supply.verified_asset(asset,cache,deadline,32),raw)
            target.write_bytes(b'corrupted')
            def download(args,**kwargs):Path(args[-2]).write_bytes(raw)
            with patch.object(deadline,'run',side_effect=download):
                self.assertEqual(supply.verified_asset(asset,cache,deadline,32),raw)
            target.unlink()
            (cache/'foreign').write_bytes(raw)
            target.symlink_to(cache/'foreign')
            with self.assertRaisesRegex(ValueError,'symlinks'):
                supply.verified_asset(asset,cache,supply.Deadline(30),32)

    def archive(self,name,kind=tarfile.REGTYPE):
        output = io.BytesIO()
        with tarfile.open(fileobj=output,mode='w:gz') as archive:
            item = tarfile.TarInfo('konflux-ci-'+'a'*40+'/'+name)
            item.type = kind
            if kind==tarfile.REGTYPE:
                item.size = 7
                archive.addfile(item,io.BytesIO(b'fixture'))
            else:
                item.linkname = '/outside'
                archive.addfile(item)
        return output.getvalue()

    def test_archive_traversal_links_and_duplicate_entries_fail_closed(self):
        for name,kind in [('dependencies/../../escape',tarfile.REGTYPE),
                          ('dependencies/link',tarfile.SYMTYPE),('dependencies/hard',tarfile.LNKTYPE)]:
            with self.subTest(name=name),tempfile.TemporaryDirectory() as directory:
                with self.assertRaisesRegex(ValueError,'Unsafe'):
                    supply.extract_dependencies(self.archive(name,kind),Path(directory),'a'*40)
        with tempfile.TemporaryDirectory() as directory:
            target=Path(directory)
            raw=self.archive('dependencies/fixture.yaml')
            supply.extract_dependencies(raw,target,'a'*40)
            self.assertEqual((target/'dependencies/fixture.yaml').read_bytes(),b'fixture')
            with self.assertRaises(FileExistsError):
                supply.extract_dependencies(raw,target,'a'*40)

    def test_deadline_exhaustion_prevents_new_processes(self):
        with patch.object(supply.time,'monotonic',side_effect=[0,31]), \
             patch.object(supply.subprocess,'run') as run:
            deadline=supply.Deadline(30)
            with self.assertRaisesRegex(ValueError,'deadline'):
                deadline.run(['never-run'])
            run.assert_not_called()


if __name__=='__main__':
    unittest.main()
