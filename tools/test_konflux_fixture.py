import copy
import io
import json
from types import SimpleNamespace
import unittest
from unittest.mock import patch
from pathlib import Path

import konflux_build_fixture as fixture
import konflux_fixture_guard as guard


def objects():
    run={'metadata':{'uid':'run','name':'build','namespace':'fixture'}}
    task={'metadata':{'uid':'task','name':'build-maven','namespace':'fixture','resourceVersion':'1',
          'ownerReferences':[{'kind':'PipelineRun','uid':'run','controller':True}],
          'finalizers':['results.tekton.dev/taskrun']},'status':{'podName':'build-pod','conditions':[{'type':'Succeeded','status':'True'}]}}
    pod={'metadata':{'uid':'pod','name':'build-pod','namespace':'fixture','ownerReferences':[{'kind':'TaskRun','uid':'task','controller':True}]},
         'spec':{'automountServiceAccountToken':False,'containers':[{'name':'maven'}],'volumes':[{'name':'artifacts','persistentVolumeClaim':{'claimName':'artifacts'}}]},
         'status':{'phase':'Succeeded','containerStatuses':[{'name':'maven','containerID':'containerd://original','restartCount':0,'state':{'terminated':{'exitCode':0}}}]}}
    return run,task,pod


class FixtureGuardTests(unittest.TestCase):
    def test_private_json_input_reaches_original_node_with_explicit_budget(self):
        calls = []
        def run(argv, **kwargs):
            calls.append((argv, kwargs))
            return SimpleNamespace(stdout='original-id\n')
        guard.owned_node_exec(run, 'podman', 'node-name', 'original-id',
                              ['/installer', 'observe-build-profile'],
                              input='private-json', capture_output=True, limit=130)
        self.assertEqual(calls[-1][0][:4], ['podman', 'exec', '-i', 'original-id'])
        self.assertEqual(calls[-1][1]['input'], 'private-json')
        self.assertEqual(calls[-1][1]['limit'], 130)

    def test_custom_installation_roots_match_installer(self):
        with patch.dict('os.environ', {'XDG_STATE_HOME':'/custom/state','XDG_CACHE_HOME':'/custom/cache'}):
            self.assertEqual(guard.installation_paths('fixture'), (Path('/custom/state/ai-sdlc/fixture'),Path('/custom/cache/ai-sdlc/tools')))

    def test_disk_failure_during_log_archival_does_not_skip_cleanup(self):
        report={};calls=[]
        def collect():raise OSError('disk full')
        guard.collect_best_effort(collect,report)
        # The production finally block continues to UID-scoped cleanup.
        def call(args,value=None):calls.append((args,value));return ''
        guard.cleanup_archived_namespace(call,'fixture','original',[],[])
        self.assertEqual(report['logCollectionFailure'],'OSError')
        self.assertEqual(calls[0][1]['preconditions'],{'uid':'original'})

    def test_cleanup_waits_for_chains_and_cas_only_archived_results(self):
        _,task,pod=objects();task['metadata']['deletionTimestamp']='now'
        tick=[0];patches=[];calls=[]
        def call(args,value=None):
            calls.append((args,value))
            if args[0]=='delete':return '{}'
            if args[:2]==['get','namespace']:
                if tick[0]>=2:return ''
                return json.dumps({'metadata':{'uid':'ns','deletionTimestamp':'now'}})
            if args[:2]==['get','taskruns']:
                snapshot=copy.deepcopy(task)
                if tick[0]==0:snapshot['metadata']['finalizers'].append('chains.tekton.dev/taskrun')
                return json.dumps({'items':[snapshot]})
            if args[:2]==['get','pods']:return json.dumps({'items':[pod]})
            if args[0]=='patch':patches.append(json.loads(args[-1]));return '{}'
            self.fail(args)
        def sleep(seconds):tick[0]+=seconds
        self.assertEqual(guard.cleanup_archived_namespace(call,'fixture','ns',['task'],['run'],monotonic=lambda:tick[0],sleep=sleep),['task'])
        self.assertEqual(calls[0][1]['preconditions'],{'uid':'ns'})
        self.assertEqual(len(patches),1)
        self.assertEqual(patches[0],guard.finalizer_patch(task,'task'))

    def test_cleanup_never_treats_api_error_or_replacement_as_absence(self):
        for case in ('api-error','replacement','not-deleting','active-writer','foreign-finalizer','unarchived'):
            with self.subTest(case=case):
                _,task,pod=objects();task['metadata']['deletionTimestamp']='now';tick=[0];patches=[]
                if case=='active-writer':pod['status']['phase']='Running'
                if case=='foreign-finalizer':task['metadata']['finalizers'].append('foreign')
                def call(args,value=None):
                    if args[0]=='delete':return '{}'
                    if args[:2]==['get','namespace']:
                        if case=='api-error':raise ValueError('API unavailable')
                        return json.dumps({'metadata':{'uid':'other' if case=='replacement' else 'ns', 'deletionTimestamp':None if case=='not-deleting' else 'now'}})
                    if args[:2]==['get','taskruns']:return json.dumps({'items':[task]})
                    if args[:2]==['get','pods']:return json.dumps({'items':[pod]})
                    patches.append(args);return '{}'
                def sleep(seconds):tick[0]+=seconds
                with self.assertRaises(ValueError):
                    guard.cleanup_archived_namespace(call,'fixture','ns',[] if case=='unarchived' else ['task'],['run'],timeout=2,monotonic=lambda:tick[0],sleep=sleep)
                self.assertEqual(patches,[])

    def test_node_replacement_between_inspect_and_exec_cannot_retarget_operation(self):
        calls=[]
        def run(args,**kwargs):
            calls.append(args)
            # The mutable name now points elsewhere; only the old ID is safe.
            return SimpleNamespace(stdout='original-id\n' if args[1]=='inspect' else '')
        guard.owned_node_exec(run,'podman','mutable-name','original-id',['mount','fixture'])
        self.assertEqual(calls[1],['podman','exec','original-id','mount','fixture'])

    def test_archive_import_keeps_stdin_and_immutable_node_identity(self):
        calls=[];stream=io.BytesIO(b'fixture')
        def run(args,**kwargs):calls.append((args,kwargs));return SimpleNamespace(stdout='original-id\n')
        guard.owned_node_exec(run,'podman','name','original-id',['ctr','images','import','-'],stdin=stream)
        self.assertEqual(calls[1][0],['podman','exec','-i','original-id','ctr','images','import','-'])
        self.assertIs(calls[1][1]['stdin'],stream)

    def test_changed_node_is_rejected_before_execution(self):
        calls=[]
        def run(args,**kwargs):calls.append(args);return SimpleNamespace(stdout='replacement\n')
        with self.assertRaises(ValueError):guard.owned_node_exec(run,'podman','name','original',['mount'])
        self.assertEqual(len(calls),1)

    def test_running_writer_waits_and_terminated_original_succeeds(self):
        run,task,pod=objects();binding=guard.run_binding(run,[task],[pod])
        pod['status']['phase']='Running'
        self.assertIsNone(guard.writer_identity(run,[task],[pod],binding))
        pod['status']['phase']='Succeeded'
        self.assertEqual(guard.writer_identity(run,[task],[pod],binding)['containers'][0]['containerID'],'containerd://original')

    def test_writer_rejects_changed_ownership_restart_credentials_and_status(self):
        for case in ('task-owner','pod-owner','pod-uid','task-uid','deleting','restart','nonzero','missing-status','secret','token','ephemeral','failed'):
            with self.subTest(case=case):
                run,task,pod=objects();binding=guard.run_binding(run,[task],[pod])
                if case=='task-owner':task['metadata']['ownerReferences'][0]['uid']='foreign'
                if case=='pod-owner':pod['metadata']['ownerReferences'][0]['uid']='foreign'
                if case=='pod-uid':pod['metadata']['uid']='replacement'
                if case=='task-uid':task['metadata']['uid']='replacement'
                if case=='deleting':pod['metadata']['deletionTimestamp']='now'
                if case=='restart':pod['status']['containerStatuses'][0]['restartCount']=1
                if case=='nonzero':pod['status']['containerStatuses'][0]['state']['terminated']['exitCode']=1
                if case=='missing-status':pod['status']['containerStatuses']=[]
                if case=='secret':pod['spec']['volumes'].append({'secret':{'secretName':'unexpected'}})
                if case=='token':pod['spec']['automountServiceAccountToken']=True
                if case=='ephemeral':pod['spec']['ephemeralContainers']=[{'name':'extra'}]
                if case=='failed':pod['status']['phase']='Failed'
                with self.assertRaises(ValueError):guard.writer_identity(run,[task],[pod],binding)

    def test_finalizer_cleanup_preserves_foreign_or_running_resources(self):
        for case in ('foreign-owner','foreign-finalizer','extra-finalizer','running','pending'):
            with self.subTest(case=case):
                _,task,pod=objects()
                if case=='foreign-owner':task['metadata']['ownerReferences'][0]['uid']='foreign'
                if case=='foreign-finalizer':task['metadata']['finalizers']=['foreign']
                if case=='extra-finalizer':task['metadata']['finalizers'].append('foreign')
                if case=='running':pod['status']['phase']='Running'
                if case=='pending':task['status']['conditions'][0]['status']='Unknown'
                self.assertFalse(guard.removable_results_finalizer(task,['run'],[pod]))
        _,task,pod=objects();self.assertTrue(guard.removable_results_finalizer(task,['run'],[pod]))
        patch=guard.finalizer_patch(task,'task');self.assertEqual([p['op'] for p in patch],['test','test','test','remove'])
        with self.assertRaises(ValueError):guard.finalizer_patch(task,'replacement')

    def test_network_gate_precedes_source_and_publisher_cannot_write_artifacts(self):
        build=fixture.build_pipeline([{'host':'10.96.0.1','port':443}])
        step=build['tasks'][0]['taskSpec']['steps'][0]
        self.assertLess(step['script'].index('timeout 3'),step['script'].index('git -c'))
        self.assertFalse(any('secret' in v for v in build['tasks'][0]['taskSpec']['volumes']))
        publish=fixture.publish_pipeline('pinned-image','definition','output')
        task=publish['tasks'][0]['taskSpec']
        self.assertTrue(task['volumes'][0]['persistentVolumeClaim']['readOnly'])
        for spec in (build['tasks'][0]['taskSpec'],task):
            self.assertFalse(any('hostPath' in v for v in spec['volumes']))
            for step in spec['steps']:
                self.assertFalse(step['securityContext']['allowPrivilegeEscalation'])
                self.assertEqual(step['securityContext']['capabilities']['drop'],['ALL'])
        with self.assertRaises(ValueError):fixture.build_pipeline([{'host':'10.1.1.1; bad','port':443}])



class OCIArchiveTests(unittest.TestCase):
    def test_blob_manifest_identity_and_unsafe_archive_members(self):
        import hashlib,json,tarfile,tempfile
        from pathlib import Path
        blob=b'{"schemaVersion":2}'
        digest='sha256:'+hashlib.sha256(blob).hexdigest()
        image='localhost/ai-sdlc-artifact-builder@'+digest
        with tempfile.TemporaryDirectory() as directory:
            archive=Path(directory)/'image.oci'
            def write(extra=None,content=blob):
                with tarfile.open(archive,'w') as output:
                    entries={'oci-layout':b'{"imageLayoutVersion":"1.0.0"}',
                             'index.json':json.dumps({'manifests':[{'digest':digest,'size':len(blob)}]}).encode(),
                             'blobs/sha256/'+digest.split(':')[1]:content}
                    for name,raw in entries.items():
                        member=tarfile.TarInfo(name);member.size=len(raw);output.addfile(member,io.BytesIO(raw))
                    if extra:
                        member=tarfile.TarInfo(extra);member.type=tarfile.SYMTYPE;member.linkname='/etc/passwd';output.addfile(member)
            write();self.assertEqual(len(guard.validate_oci_archive(archive,image)),64)
            with self.assertRaises(ValueError):guard.validate_oci_archive(archive,image[:-1]+'0')
            write(content=b'changed')
            with self.assertRaises(ValueError):guard.validate_oci_archive(archive,image)
            write(extra='linked')
            with self.assertRaises(ValueError):guard.validate_oci_archive(archive,image)

if __name__=='__main__':unittest.main()
