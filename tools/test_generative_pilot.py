import json
import os
import subprocess
import sys
from pathlib import Path
import tempfile
import unittest

from verify_generative_pilot import (CASES, CANDIDATE_DRIVER, EXISTING_TEST,
                                    LocaleBenchmark, NEW_TEST, SOURCE_SHA, UTILS,
                                    WRAPPER_FIXTURE, bounded_container, parse_results, stage_oracle_inputs)
from verify_workspace_bundle import source_bundle
import base64
import contextlib
import io
from unittest.mock import Mock, patch
import verify_generative_pilot as pilot


class ResultTests(unittest.TestCase):
    def test_cleanup_or_trace_failure_cannot_return_cli_success(self):
        for mode in ('invalid-trace', 'oversized-trace', 'forced-kill'):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                output = Path(directory)/'report'
                args = ['fixture', '--provider', 'podman']
                for name in ('kubectl','kubeconfig','test-binary','source-repository'):
                    args += ['--'+name, '/unused']
                for name in ('render-image','validation-image','mailbox-image','worker-image'):
                    args += ['--'+name, 'sha256:'+'a'*64]
                args += ['--output', str(output)]
                process = Mock()
                process.poll.side_effect = [None, 0]
                process.wait.side_effect = [subprocess.TimeoutExpired('owned-wrapper', 5), -9] if mode=='forced-kill' else [0]
                def start(*_args, **kwargs):
                    if mode=='invalid-trace': kwargs['stdout'].write(b'{invalid')
                    if mode=='oversized-trace': kwargs['stdout'].write(b'x'*8193)
                    return process
                response = Mock()
                response.status = 200
                transport = Mock(metadata=[], ledger=None)
                with patch.object(sys,'argv',args), patch.object(pilot,'source_bundle',return_value={}), \
                     patch.object(pilot.LocaleBenchmark,'baseline'), patch.object(pilot,'RealTransport',return_value=transport), \
                     patch.object(pilot,'verify',return_value={}), patch.object(pilot.subprocess,'Popen',side_effect=start), \
                     patch.object(pilot.urllib.request,'urlopen') as opened, contextlib.redirect_stdout(io.StringIO()):
                    opened.return_value.__enter__.return_value=response
                    self.assertEqual(pilot.main(),1)
                self.assertEqual(json.loads((output/'report.json').read_text())['status'],'failed')

    def test_wrapper_measurements_exclude_request_response_and_exception_text(self):
        with tempfile.TemporaryDirectory() as directory:
            Path(directory, 'codexcli_openai_wrapper.py').write_text('''
def complete(request):
    if request.get('fail'): raise RuntimeError('private-exception-canary')
    return {'content':'private-response-canary'}
def backend_binary(): pass
def policy_snapshot(): pass
class Handler: pass
class HTTPServer:
    def __init__(self, *args): pass
    def serve_forever(self):
        complete({'content':'private-request-canary'})
        try: complete({'fail':True})
        except RuntimeError: pass
''')
            result = subprocess.run([sys.executable, '-c', WRAPPER_FIXTURE, directory],
                                    env=dict(os.environ, CODEX_WRAPPER_PORT='1'), capture_output=True, timeout=3)
            self.assertEqual(result.returncode, 0)
            self.assertNotIn(b'canary', result.stdout)
            records = [json.loads(line) for line in result.stdout.splitlines()]
            self.assertEqual([item['outcome'] for item in records], ['completed', 'failed'])
            self.assertTrue(all(set(item)=={'outcome','seconds'} for item in records))

    def test_missing_partial_extra_or_nonstring_results_are_rejected(self):
        for raw in (b'', b'[]', b'null', b'{}', json.dumps(['']*(len(CASES)-1)).encode(),
                    json.dumps(['']*(len(CASES)+1)).encode(), json.dumps([None]*len(CASES)).encode(),
                    json.dumps(['x'*1025]*len(CASES)).encode()):
            with self.subTest(raw=raw[:30]), self.assertRaises(ValueError):
                parse_results(raw)
        self.assertEqual(parse_results(json.dumps(['']*len(CASES)).encode()), ['']*len(CASES))


@unittest.skipUnless(os.environ.get('AI_SDLC_TEST_ORACLE_IMAGE'), 'explicit offline OCI oracle fixture required')
class RealOracleTests(unittest.TestCase):
    def setUp(self):
        self.provider = os.environ.get('AI_SDLC_TEST_ORACLE_PROVIDER', 'podman')
        self.image = os.environ['AI_SDLC_TEST_ORACLE_IMAGE']

    def test_exit_zero_is_not_accepted_as_results(self):
        code, raw, _ = bounded_container(self.provider, ['--rm', '--pull', 'never', '--network', 'none',
            '--read-only', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
            '--entrypoint', 'node', self.image, '-e', 'process.exit(0)'])
        self.assertEqual(code, 0)
        with self.assertRaises(ValueError): parse_results(raw)

    def test_realm_escape_cannot_bypass_external_comparison(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            target = root / UTILS
            target.parent.mkdir(parents=True)
            target.write_text("window.constructor.constructor('return process')().exit(0)")
            (root/'cases.json').write_text(json.dumps(CASES))
            (root/'candidate.cjs').write_text(CANDIDATE_DRIVER)
            code, raw, _ = bounded_container(self.provider, ['--rm', '--pull', 'never', '--network', 'none',
                '--read-only', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
                '-v', str(root)+':/check:ro,z', '--entrypoint', 'node', self.image, '/check/candidate.cjs'])
            self.assertNotEqual(code, 0)
            with self.assertRaises(ValueError): parse_results(raw)

    def test_candidate_has_no_reference_answers_or_harness_input(self):
        with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as reference_directory:
            root, reference = Path(directory), Path(reference_directory)
            source = b"window.HomeDirUtils={formatDate:()=>typeof input==='undefined'?'no-input':input.locale};"
            stage_oracle_inputs(root, reference, {UTILS:source})
            self.assertFalse((root/'reference-driver.cjs').exists())
            self.assertTrue((reference/'reference-driver.cjs').is_file())
            self.assertTrue(all(set(case)=={'lang','browser','raw','opts'} for case in json.loads((root/'cases.json').read_text())))
            code, raw, _ = bounded_container(self.provider, ['--rm', '--pull', 'never', '--network', 'none',
                '--read-only', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
                '-v', str(root)+':/check:ro,z', '--entrypoint', 'node', self.image, '/check/candidate-driver.cjs'])
            self.assertEqual(code,0)
            self.assertEqual(parse_results(raw), ['no-input']*len(CASES))

    def test_generated_exit_only_test_is_not_regression_coverage(self):
        repository = os.environ.get('AI_SDLC_TEST_HOMEDIR_SOURCE')
        if not repository: self.skipTest('pinned Homedir checkout required')
        bundle = source_bundle(Path(repository), SOURCE_SHA)
        files = {item['path']: base64.b64decode(item['data']) for item in bundle['files']
                 if item['path'] in (UTILS, EXISTING_TEST)}
        files[NEW_TEST] = b'process.exit(0);\n'
        with tempfile.TemporaryDirectory() as directory:
            benchmark = LocaleBenchmark(self.provider, self.image, Path(directory))
            with self.assertRaisesRegex(ValueError, 'execution evidence missing or insufficient'):
                benchmark.run_checks(files)

    def test_timeout_removes_owned_container(self):
        identity = pilot.secrets.token_hex(16)
        with patch.object(pilot.secrets,'token_hex',return_value=identity):
            with self.assertRaises(subprocess.TimeoutExpired) as failure:
                bounded_container(self.provider, ['--rm', '--pull', 'never', '--network', 'none',
                    '--read-only', '--entrypoint', 'node', self.image, '-e', 'setInterval(()=>{},1000)'], timeout=1)
        self.assertEqual(failure.exception.cmd[1], 'run', 'cleanup timeout cannot impersonate execution timeout')
        observed = subprocess.run([self.provider,'ps','-a','--no-trunc','--filter',
                                   'label=ai-sdlc.fixture='+identity,'--format','{{.ID}}'],
                                  capture_output=True,timeout=5)
        self.assertEqual(observed.returncode,0)
        self.assertEqual(observed.stdout,b'', 'timed-out fixture container survived cleanup')

    def test_generated_output_cannot_exceed_capture_bound(self):
        with self.assertRaisesRegex(ValueError, 'output exceeded bound'):
            bounded_container(self.provider, ['--rm', '--pull', 'never', '--network', 'none',
                '--read-only', '--entrypoint', 'node', self.image, '-e',
                'process.stdout.write(Buffer.alloc(2*1024*1024,120))'], timeout=5)

class CleanupRaceTests(unittest.TestCase):
    def test_cid_removed_before_open_requires_healthy_label_absence(self):
        identity = 'a'*32
        real_open = Path.open
        for healthy in (True, False):
            calls = []
            def disappearing_open(path, *args, **kwargs):
                if path.name == 'container.id' and args == ('rb',):
                    path.unlink()
                return real_open(path, *args, **kwargs)
            def execute(command, **kwargs):
                calls.append(command)
                if command[1] == 'run':
                    Path(command[command.index('--cidfile')+1]).write_text('b'*64)
                    kwargs['stdout'].write(b'x'*(1024*1024))
                    return subprocess.CompletedProcess(command, 1)
                if command[1] == 'inspect':
                    return subprocess.CompletedProcess(command, 1, b'', b'')
                if command[1] == 'ps':
                    return subprocess.CompletedProcess(command, 0 if healthy else 1, b'', b'')
                self.fail('unexpected cleanup mutation')
            with self.subTest(healthy=healthy), patch.object(Path, 'open', disappearing_open), \
                 patch.object(pilot.secrets, 'token_hex', return_value=identity), \
                 patch.object(pilot.subprocess, 'run', side_effect=execute):
                with self.assertRaisesRegex(ValueError, 'output exceeded bound' if healthy else 'cleanup could not be confirmed'):
                    bounded_container('fixture-engine', ['--rm', 'fixture-image'])
            self.assertEqual(calls[-1][1], 'ps')
            self.assertIn('label=ai-sdlc.fixture='+identity, calls[-1])

    def test_automatic_removal_race_preserves_output_rejection(self):
        identity, container_id = 'a'*32, 'b'*64
        calls = []
        def execute(command, **kwargs):
            calls.append(command)
            if command[1] == 'run':
                Path(command[command.index('--cidfile')+1]).write_text(container_id)
                kwargs['stdout'].write(b'x'*(1024*1024))
                return subprocess.CompletedProcess(command, 1)
            if command[1] == 'inspect':
                return subprocess.CompletedProcess(command, 0, json.dumps([{'Id': container_id,
                    'Config': {'Labels': {'ai-sdlc.fixture': identity}}}]).encode(), b'')
            if command[1] == 'rm':
                return subprocess.CompletedProcess(command, 1, b'', b'removal already in progress')
            if command[1] == 'ps':
                return subprocess.CompletedProcess(command, 0, b'', b'')
            self.fail('unexpected fixture command')
        with patch.object(pilot.secrets, 'token_hex', return_value=identity), \
             patch.object(pilot.subprocess, 'run', side_effect=execute):
            with self.assertRaisesRegex(ValueError, 'output exceeded bound'):
                bounded_container('fixture-engine', ['--rm', 'fixture-image'])
        self.assertEqual(sum(command[1]=='rm' for command in calls), 1)
        self.assertTrue(any(command[1]=='ps' for command in calls))

    def test_absence_confirmation_rejects_engine_failure_and_still_present_id(self):
        for mode in ('engine-failed', 'still-present', 'wrong-id', 'invalid-output'):
            with self.subTest(mode=mode):
                identity, cid = 'a'*32, 'b'*64
                output = {'engine-failed': b'', 'still-present': (cid+'\n').encode(),
                          'wrong-id': ('c'*64+'\n').encode(), 'invalid-output': b'invalid\n'}[mode]
                result = subprocess.CompletedProcess([], 1 if mode=='engine-failed' else 0, output, b'')
                with patch.object(pilot.subprocess, 'run', return_value=result), \
                     patch.object(pilot.time, 'monotonic', side_effect=[0,0,6]):
                    with self.assertRaisesRegex(ValueError, 'cleanup'):
                        pilot.confirm_owned_absence('fixture-engine',identity,cid)

    def test_absence_confirmation_waits_read_only_for_exact_id(self):
        cid = 'b'*64
        responses = [subprocess.CompletedProcess([],0,(cid+'\n').encode(),b''),
                     subprocess.CompletedProcess([],0,b'',b'')]
        with patch.object(pilot.subprocess,'run',side_effect=responses) as run, patch.object(pilot.time,'sleep'):
            pilot.confirm_owned_absence('fixture-engine','a'*32,cid)
        self.assertEqual(run.call_count,2)
        for call in run.call_args_list:
            self.assertEqual(call.args[0][1],'ps')
            self.assertIn('id='+cid,call.args[0])

    def test_unknown_cid_still_requires_healthy_label_absence(self):
        with patch.object(pilot.subprocess,'run',return_value=subprocess.CompletedProcess([],1,b'',b'')) as run:
            with self.assertRaisesRegex(ValueError,'cleanup'):
                pilot.confirm_owned_absence('fixture-engine','a'*32,None)
        self.assertIn('label=ai-sdlc.fixture='+'a'*32,run.call_args.args[0])


if __name__ == '__main__':
    unittest.main()
