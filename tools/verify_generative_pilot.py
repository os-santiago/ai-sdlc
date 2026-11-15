#!/usr/bin/env python3
"""Opt-in real-model Homedir component benchmark; never advances live work."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import resource
import secrets
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request

from verify_executor import verify
from verify_pod_transport import TransportFixture
from verify_workspace_bundle import identities, source_bundle, wire

SOURCE_SHA = '0622cdaeb59a11a27bd5f3d7e18183c401a5e412'
UTILS = 'quarkus-app/src/main/resources/META-INF/resources/js/utils.js'
EXISTING_TEST = 'tests/js/utils-format-date.test.js'
NEW_TEST = 'tests/js/utils-locale-fallback.test.js'
CASES = [
    {"lang": lang, "browser": browser, "locale": locale, "raw": "2026-03-15T13:45:00Z", "opts": opts}
    for lang, browser, locale, opts in [
        ('bad_locale', 'es', 'es', None), ('en_US', 'en-US', 'en-US', None),
        ('bad_locale', 'also_bad', 'en', None), ('', 'bad_locale', 'en', None),
        ('es', 'en', 'es', None), ('en', 'es', 'en', None), ('', 'es', 'es', None),
        ('bad_locale', 'es', 'es', 'datetime'), ('bad_locale', 'es', 'es', 'short'),
        ('bad_locale', 'es', 'es', {'weekday': 'long'}),
        ('en', 'es', 'en', {'month': 'bad-option'})]
] + [{'lang':'en', 'browser':'es', 'locale':'en', 'raw':raw, 'opts':None}
     for raw in [None, '', 'not-a-date', 0]]

# The untrusted process only produces values. A separate trusted invocation
# computes expected Intl values, and Python compares outside both containers.
CANDIDATE_DRIVER = r''' 
const fs = require('node:fs');
const vm = require('node:vm');
const cases = JSON.parse(fs.readFileSync('/check/cases.json', 'utf8'));
const source = fs.readFileSync('/check/quarkus-app/src/main/resources/META-INF/resources/js/utils.js', 'utf8');
const results = cases.map(item => {
  const context = vm.createContext(Object.create(null), {codeGeneration:{strings:false, wasm:false}});
  vm.runInContext('var window={}; var document={documentElement:{lang:'+JSON.stringify(item.lang)+'}}; var navigator={language:'+JSON.stringify(item.browser)+'};', context, {timeout:500});
  vm.runInContext(source, context, {timeout:500});
  return vm.runInContext('window.HomeDirUtils.formatDate('+JSON.stringify(item.raw)+','+JSON.stringify(item.opts)+')', context, {timeout:500});
});
process.stdout.write(JSON.stringify(results));
'''
REFERENCE_DRIVER = r'''
const fs = require('node:fs');
const cases = JSON.parse(fs.readFileSync('/check/cases.json', 'utf8'));
const presets = {date:{year:'numeric',month:'short',day:'numeric'}, datetime:{month:'short',day:'2-digit',hour:'2-digit',minute:'2-digit'}, short:{month:'short',day:'2-digit'}};
const results = cases.map(item => {
  if (!item.raw) return '';
  try {
    const date = new Date(item.raw);
    if (isNaN(date.getTime())) return '';
    const options = typeof item.opts === 'string' ? presets[item.opts] || presets.date : item.opts || presets.date;
    return options.hour || options.minute ? date.toLocaleString(item.locale, options) : date.toLocaleDateString(item.locale, options);
  } catch (_) { return ''; }
});
process.stdout.write(JSON.stringify(results));
'''


WRAPPER_FIXTURE = r'''
import json, os, sys, time
sys.path.insert(0, sys.argv[1])
import codexcli_openai_wrapper as wrapper
original = wrapper.complete
def measured(request):
    started = time.monotonic()
    outcome = 'failed'
    try:
        result = original(request)
        outcome = 'completed'
        return result
    except Exception as error:
        if str(error) == 'completion backend deadline exceeded': outcome = 'backend-deadline'
        elif isinstance(error, ValueError): outcome = 'rejected'
        raise
    finally:
        print(json.dumps({'outcome':outcome, 'seconds':round(time.monotonic()-started,3)}), flush=True)
wrapper.complete = measured
wrapper.backend_binary()
wrapper.policy_snapshot()
wrapper.HTTPServer(('127.0.0.1', int(os.environ['CODEX_WRAPPER_PORT'])), wrapper.Handler).serve_forever()
'''


def parse_results(raw):
    if len(raw)>32*1024: raise ValueError('Independent result bound')
    try: result=json.loads(raw)
    except (ValueError, UnicodeError): raise ValueError('Independent result is not JSON') from None
    if not isinstance(result,list) or len(result)!=len(CASES) or any(not isinstance(x,str) or len(x)>1024 for x in result):
        raise ValueError('Independent result shape')
    return result



def bounded_container(provider, arguments, timeout=30):
    """Bound untrusted test output and remove only this invocation's container."""
    identity = secrets.token_hex(16)
    name = 'ai-sdlc-locale-check-' + identity
    label = 'ai-sdlc.fixture=' + identity
    with tempfile.TemporaryDirectory(prefix='ai-sdlc-oracle-output-') as directory:
        cid = Path(directory) / 'container.id'
        def bounds():
            resource.setrlimit(resource.RLIMIT_FSIZE, (1024*1024, 1024*1024))
        try:
            with (Path(directory)/'stdout').open('w+b') as stdout, (Path(directory)/'stderr').open('w+b') as stderr:
                result = subprocess.run([provider, 'run', '--name', name, '--label', label,
                                         '--cidfile', str(cid), '--log-driver', 'none'] + arguments,
                                        stdout=stdout, stderr=stderr, timeout=timeout, preexec_fn=bounds)
                if stdout.tell() >= 1024*1024 or stderr.tell() >= 1024*1024:
                    raise ValueError('Independent test output exceeded bound')
                stdout.seek(0)
                stderr.seek(0)
                return result.returncode, stdout.read(64*1024), stderr.read(64*1024)
        finally:
            recorded_id = None
            if cid.exists():
                with cid.open('rb') as stream:
                    recorded = stream.read(129).strip()
                if not re.fullmatch(b'[a-f0-9]{64}', recorded):
                    raise ValueError('Independent test cleanup identity invalid')
                recorded_id = recorded.decode('ascii')
            try:
                observed = subprocess.run([provider, 'inspect', name], capture_output=True, timeout=5)
            except (subprocess.TimeoutExpired, OSError):
                raise ValueError('Independent test cleanup could not be confirmed') from None
            if observed.returncode == 0:
                try:
                    if len(observed.stdout)>64*1024:
                        raise ValueError()
                    info = json.loads(observed.stdout)[0]
                    observed_id = info['Id']
                    owned = info['Config'].get('Labels', {}).get('ai-sdlc.fixture') == identity
                except (ValueError, KeyError, TypeError, IndexError, AttributeError):
                    raise ValueError('Independent test cleanup identity invalid') from None
                if not owned or not isinstance(observed_id,str) or not re.fullmatch('[a-f0-9]{64}',observed_id) or (recorded_id and recorded_id!=observed_id):
                    raise ValueError('Independent test cleanup identity mismatch')
                recorded_id = observed_id
                # --rm may already be removing this exact container. Never retry
                # DELETE; only a healthy engine's confirmed absence can resolve it.
                try:
                    subprocess.run([provider, 'rm', '-f', '-v'] + (['--time', '0'] if Path(provider).name=='podman' else []) + [observed_id], capture_output=True, timeout=10)
                except subprocess.TimeoutExpired:
                    pass
                except OSError:
                    raise ValueError('Independent test cleanup could not be confirmed') from None
            confirm_owned_absence(provider, identity, recorded_id)


def confirm_owned_absence(provider, identity, container_id):
    deadline = time.monotonic()+5
    selector = 'id='+container_id if container_id else 'label=ai-sdlc.fixture='+identity
    while True:
        remaining = deadline-time.monotonic()
        if remaining<=0:
            raise ValueError('Independent test cleanup could not be confirmed')
        try:
            result = subprocess.run([provider, 'ps', '-a', '--no-trunc', '--filter', selector, '--format', '{{.ID}}'],
                                    capture_output=True, timeout=remaining)
        except (subprocess.TimeoutExpired, OSError):
            raise ValueError('Independent test cleanup could not be confirmed') from None
        if result.returncode or len(result.stdout)>4096:
            raise ValueError('Independent test cleanup could not be confirmed')
        identifiers = result.stdout.splitlines()
        if not identifiers:
            return
        if any(not re.fullmatch(b'[a-f0-9]{64}',value) for value in identifiers) or (container_id and identifiers!=[container_id.encode()]):
            raise ValueError('Independent test cleanup identity mismatch')
        remaining = deadline-time.monotonic()
        if remaining<=0:
            raise ValueError('Independent test cleanup could not be confirmed')
        time.sleep(min(.1,remaining))


def stage_oracle_inputs(root, reference, files):
    for name, data in files.items():
        target = root / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
        target.chmod(0o444)
    (root / 'candidate-driver.cjs').write_text(CANDIDATE_DRIVER)
    reference.chmod(0o755)
    (reference / 'reference-driver.cjs').write_text(REFERENCE_DRIVER)
    (reference / 'cases.json').write_text(json.dumps(CASES))
    (root / 'cases.json').write_text(json.dumps([{key:value for key,value in case.items() if key!='locale'} for case in CASES]))
    root.chmod(0o755)


class LocaleBenchmark:
    def __init__(self, provider, image, output):
        self.provider, self.image, self.output = provider, image, output

    def assignment(self):
        return dict(title='Isolated benchmark: keep Homedir dates visible with malformed page locale',
                    body=('Synthetic benchmark, not a live issue. In '+UTILS+', formatDate currently returns an empty string when document.documentElement.lang is malformed. Improve date visibility by selecting the first structurally valid locale from document language, navigator.language, then English. Preserve valid locale preference, presets, custom options, invalid-date behavior and invalid-options behavior. Add regression coverage in '+NEW_TEST+'. Read AGENTS.md and the relevant existing source/tests. Only those two files may change. Do not modify automation. No network or credentials are available.'),
                    acceptanceCriteria=[
                        'Malformed document language falls back to valid navigator language; if both are malformed, use English.',
                        'Existing formatDate tests pass and new tests cover fallback without masking invalid formatting options.',
                        'Change only '+UTILS+' and add '+NEW_TEST+'. Preserve every other source file.'])

    def run_checks(self, files, baseline=False):
        with tempfile.TemporaryDirectory(prefix='ai-sdlc-locale-oracle-') as directory, tempfile.TemporaryDirectory(prefix='ai-sdlc-locale-reference-') as reference_directory:
            root = Path(directory)
            reference = Path(reference_directory)
            stage_oracle_inputs(root, reference, files)
            common = ['--rm', '--pull', 'never', '--network', 'none',
                      '--read-only', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
                      '--pids-limit', '64', '--memory', '256m', '--cpus', '1', '--user', '1000:1000',
                      '--tmpfs', '/tmp:rw,noexec,nosuid,size=16m', '-v', str(root)+':/check:ro,z',
                      '--env', 'TZ=UTC', '--workdir', '/check', '--entrypoint', 'node', self.image]
            tests = ['/check/'+EXISTING_TEST]
            if NEW_TEST in files: tests.append('/check/'+NEW_TEST)
            code, test_output, _ = bounded_container(self.provider, common+['--test']+tests)
            if code:
                raise ValueError('Independent existing/generated date tests failed')
            # Test-runner output is auxiliary evidence from untrusted generated
            # tests. A process.exit(0)-only file is not regression coverage.
            counts = re.findall(rb'^# tests ([0-9]+)$', test_output, re.MULTILINE)
            if len(counts)!=1 or int(counts[0]) < (8 if baseline else 10):
                raise ValueError('Date test execution evidence missing or insufficient')
            reference_args = [str(reference)+':/check:ro,z' if part==str(root)+':/check:ro,z' else part for part in common]
            code, expected_raw, _ = bounded_container(self.provider, reference_args+['/check/reference-driver.cjs'])
            if code: raise ValueError('Trusted reference execution failed')
            expected = parse_results(expected_raw)
            code, actual_raw, _ = bounded_container(self.provider, common+['/check/candidate-driver.cjs'])
            if code: raise ValueError('Candidate evaluation failed')
            actual = parse_results(actual_raw)
            mismatches = [i for i,(left,right) in enumerate(zip(actual,expected)) if left!=right]
            if baseline:
                if 0 not in mismatches or actual[0] != '' or not expected[0]:
                    raise ValueError('Baseline did not reproduce the specified locale defect')
            elif mismatches:
                raise ValueError('Independent candidate locale oracle failed: case indices '+str(mismatches))
            return dict(existingAndGeneratedTestsPassed=True, auxiliaryReportedTests=int(counts[0]),
                        externalComparisonPassed=not baseline, casesCompared=len(CASES),
                        baselineDefectReproduced=baseline)

    def baseline(self, bundle):
        files = {item['path']: base64.b64decode(item['data'], validate=True) for item in bundle['files'] if item['path'] in (UTILS, EXISTING_TEST)}
        self.run_checks(files, baseline=True)

    def validate(self, original, candidate):
        before, after = identities(original), identities(candidate)
        changed = sorted(name for name in set(before)|set(after) if before.get(name) != after.get(name))
        if changed != sorted([UTILS, NEW_TEST]) or NEW_TEST in before:
            raise ValueError('Candidate did not preserve the exact allowed-file boundary')
        files = {item['path']: base64.b64decode(item['data'], validate=True) for item in candidate['files'] if item['path'] in (UTILS, EXISTING_TEST, NEW_TEST)}
        if any(len(data)>32*1024 for data in files.values()):
            raise ValueError('Candidate test/source size exceeded benchmark bound')
        checks = self.run_checks(files)
        for name in (UTILS, NEW_TEST):
            target = self.output / 'candidate' / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(files[name])
        return dict(benchmark='homedir-date-locale-fallback/v1', changedFiles=changed,
                    unmodifiedSourceIdentitiesPreserved=True, baselineDefectReproduced=True,
                    candidateIndependentChecks=checks,
                    candidateFileSHA256={name: hashlib.sha256(files[name]).hexdigest() for name in changed})


class RealTransport(TransportFixture):
    def __init__(self, kubectl, binary, endpoint, token):
        super().__init__(kubectl, binary)
        self.endpoint, self.token = endpoint, token
        self.sequences = set()
        self.metadata = []
        self.ledger = None

    def run(self, scenario, config=None, replacement_uid=None):
        if scenario != 'normal':
            return super().run(scenario, config, replacement_uid)
        payload = {'Config': config or self.config, 'Binding': self.binding,
                   'DatabaseURL': self.database, 'Endpoint': self.endpoint, 'Token': self.token}
        with tempfile.TemporaryDirectory(prefix='ai-sdlc-generative-control-') as directory:
            path = Path(directory) / 'input.json'
            fd = os.open(path, os.O_WRONLY|os.O_CREAT|os.O_EXCL, 0o600)
            with os.fdopen(fd, 'wb') as stream: stream.write(wire(payload))
            env = dict(os.environ, AI_SDLC_GENERATIVE_FIXTURE=str(path))
            result = subprocess.run([self.test_binary, '-test.run', '^TestRealCodexKubernetesMailboxFixture$', '-test.v'],
                                    env=env, capture_output=True, timeout=190)
            self.metadata = [match.group(0).decode() for match in re.finditer(rb'completion sequence=[0-9]+ reason=[a-z_]+ proposedTools=\[[A-Za-z0-9_ ,]*\]|tool-result name=[A-Za-z0-9_]+ class=[a-z-]+', result.stdout)]
            proof = re.search(rb'GENERATIVE_LEDGER completed=([0-8]) handed=([0-8]) uncertain=([0-8])\b', result.stdout)
            if proof:
                self.ledger = dict(zip(('completed','handedOff','uncertain'), map(int,proof.groups())))
            if result.returncode:
                raise ValueError('Real model broker fixture failed: '+result.stdout.decode(errors='replace')[:1200])
            match = re.search(rb'GENERATIVE_HANDOFFS=([2-8])\b', result.stdout)
            if not match: raise ValueError('Real model durable handoff count missing')
            self.sequences = set(range(1, int(match.group(1))+1))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('kubectl', 'kubeconfig', 'render-image', 'mailbox-image', 'worker-image', 'validation-image', 'test-binary', 'source-repository', 'output'):
        parser.add_argument('--'+name, required=True)
    parser.add_argument('--provider', choices=('docker', 'podman'), required=True)
    args = parser.parse_args()
    output = Path(args.output).resolve()
    output.mkdir(mode=0o700, parents=False, exist_ok=False)
    for image in (args.render_image, args.validation_image):
        if not re.fullmatch(r'(?:sha256:|[A-Za-z0-9./:_-]+@sha256:)[a-f0-9]{64}', image):
            raise ValueError('Render and validation images require immutable identities')
    started = time.monotonic()
    report = {'scope':'isolated-real-model-component-benchmark', 'liveWorkflowAdvanced':False, 'status':'failed',
              'validationImage':args.validation_image, 'renderImage':args.render_image}
    process = None
    transport = None
    wrapper_trace = tempfile.TemporaryFile()
    def expired(_signal, _frame):
        signal.alarm(0)
        raise TimeoutError('Overall generative fixture deadline')
    previous = signal.signal(signal.SIGALRM, expired)
    signal.alarm(600)
    try:
        bundle = source_bundle(Path(args.source_repository), SOURCE_SHA)
        benchmark = LocaleBenchmark(args.provider, args.validation_image, output)
        benchmark.baseline(bundle)
        report['baselineDefectReproduced'] = True
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        token = secrets.token_urlsafe(48)
        wrapper = Path(__file__).resolve().parents[1] / 'runtime/k8s/tools/codexcli_openai_wrapper.py'
        env = dict(os.environ, CODEX_WRAPPER_TOKEN=token, CODEX_WRAPPER_PORT=str(port))
        process = subprocess.Popen([sys.executable, '-c', WRAPPER_FIXTURE, str(wrapper.parent)], env=env, stdout=wrapper_trace, stderr=subprocess.DEVNULL)
        deadline = time.monotonic()+10
        while True:
            if process.poll() is not None: raise ValueError('Owned Codex wrapper failed startup')
            try:
                with urllib.request.urlopen(f'http://127.0.0.1:{port}/healthz', timeout=1) as response:
                    if response.status == 200: break
            except OSError:
                if time.monotonic()>=deadline: raise ValueError('Owned Codex wrapper readiness deadline')
                time.sleep(.1)
        kubectl = [args.kubectl, '--kubeconfig', args.kubeconfig]
        transport = RealTransport(kubectl, args.test_binary, f'http://127.0.0.1:{port}/v1/chat/completions', token)
        report.update(verify(kubectl, args.provider, args.render_image, args.mailbox_image, args.worker_image,
                             bundle, transport=transport, benchmark=benchmark))
        report.update(status='passed', scope='isolated-real-model-component-benchmark')
    finally:
        signal.alarm(0)
        signal.signal(signal.SIGALRM, previous)
        if process is not None:
            process.send_signal(signal.SIGINT)  # Let the adapter's child-group finally cleanup run.
            try: process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill(); process.wait(timeout=3)
                report['status'] = 'failed'
                report['wrapperForcedKill'] = True
        wrapper_trace.seek(0)
        trace_raw = wrapper_trace.read(8193)
        wrapper_trace.close()
        if len(trace_raw)>8192:
            report['status']='failed'
        else:
            try:
                report['completionTimings']=[json.loads(line) for line in trace_raw.splitlines()]
            except ValueError:
                report.update(status='failed', incompleteWrapperTrace=True)
        if transport is not None:
            report['completionMetadata'] = transport.metadata
            report['modelLedger'] = transport.ledger
            report['durableModelHandoffs'] = transport.ledger['handedOff'] if transport.ledger else None
        report.update(ownedWrapperRemoved=process is not None and process.poll() is not None,
                      totalSeconds=round(time.monotonic()-started,3))
        (output/'report.json').write_text(json.dumps(report, indent=2)+'\n')
    print(json.dumps(report, indent=2))
    return 0 if report['status'] == 'passed' else 1


if __name__ == '__main__':
    raise SystemExit(main())
