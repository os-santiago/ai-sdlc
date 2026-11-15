#!/usr/bin/env python3
"""Compose signed fixture ownership, fixed Kubernetes effects and model handoff."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import tempfile
import time
import urllib.parse

import network_probe
from verify_executor import FIXTURE_BODY, FIXTURE_PATH, tree_digest
from verify_pod_transport import TransportFixture
from verify_workspace_bundle import identities, source_bundle, wire


def verify(kubectl, worker_image, mailbox_image, test_binary, bundle):
    database = urllib.parse.urlsplit(os.environ.get('AI_SDLC_TEST_DATABASE_URL', ''))
    if database.scheme not in ('postgres', 'postgresql') or database.hostname not in ('127.0.0.1', 'localhost', '::1') or database.path != '/ai_sdlc_test':
        raise ValueError('Explicit local disposable ai_sdlc_test database is required; fixture resets it')
    started = time.monotonic()
    namespace = 'ai-sdlc-lifecyclecheck-' + secrets.token_hex(6)
    namespace_uid = None
    process = None
    transport = TransportFixture(kubectl, test_binary)
    if FIXTURE_PATH in identities(bundle):
        raise ValueError('Source already contains fixture file')
    adev = Path(__file__).resolve().parents[1].joinpath('runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime/files/adev-profile.yaml').read_bytes()
    profile = wire({'version': 'ai-sdlc.executor-profile/v1', 'model': 'fixture-model',
                    'maxSteps': 4, 'maxSeconds': 45, 'maxTokens': 2048,
                    'aDevSHA256': hashlib.sha256(adev).hexdigest()})

    def command(args, value=None, expected=0, timeout=45):
        result = subprocess.run(kubectl + args, input=wire(value) if isinstance(value, dict) else value,
                                capture_output=True, timeout=timeout)
        if result.returncode != expected:
            raise ValueError('Lifecycle fixture Kubernetes bootstrap failed')
        return result.stdout

    def private(path, raw):
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, 'wb') as stream:
            stream.write(raw)

    try:
        created = json.loads(command(['create', '-f', '-', '-o', 'json'], {
            'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
            'labels': {'app.kubernetes.io/part-of': 'ai-sdlc-lifecycle-conformance',
                       'pod-security.kubernetes.io/enforce': 'restricted'}}}))
        namespace_uid = created['metadata']['uid']
        command(['create', '-f', '-'], {'apiVersion': 'v1', 'kind': 'ServiceAccount',
                'metadata': {'name': 'lifecycle-controller', 'namespace': namespace},
                'automountServiceAccountToken': False})
        token = command(['-n', namespace, 'create', 'token', 'lifecycle-controller', '--duration=10m']).decode().strip()
        config = {'APIServer': transport.config['APIServer'], 'CA': transport.config['CA'], 'BearerToken': token}
        with tempfile.TemporaryDirectory(prefix='ai-sdlc-lifecycle-fixture-') as directory:
            root = Path(directory)
            source, output, report = root/'source.json', root/'candidate.json', root/'report.json'
            plan, ready, control = root/'plan.json', root/'ready', root/'control.json'
            private(source, wire(bundle))
            private(control, wire({'Config': config, 'Placement': {'Namespace': namespace,
                'WorkerImage': worker_image, 'MailboxImage': mailbox_image},
                'Profile': base64.b64encode(profile).decode(), 'ADev': base64.b64encode(adev).decode(),
                'SourceFile': str(source), 'OutputFile': str(output), 'ReportFile': str(report),
                'PlanFile': str(plan), 'ReadyFile': str(ready)}))
            with (root/'driver.log').open('w+b') as log:
                process = subprocess.Popen([str(Path(test_binary).resolve()), '-test.run', '^TestRealLifecycleModelFixture$', '-test.v'],
                    env=dict(os.environ, AI_SDLC_LIFECYCLE_FIXTURE=str(control)), stdout=log, stderr=subprocess.STDOUT)
                deadline = time.monotonic()+20
                planned = None
                while time.monotonic()<deadline and process.poll() is None:
                    if plan.exists():
                        with plan.open('rb') as stream:
                            raw = stream.read(2049)
                        if len(raw)>2048:
                            raise ValueError('Fixture plan bound')
                        try:
                            planned = json.loads(raw)
                            break
                        except json.JSONDecodeError:
                            pass  # The trusted driver may still be finishing its small write.
                    time.sleep(.025)
                if not isinstance(planned, dict) or set(planned)!={'namespace','pod','config'} or planned['namespace']!=namespace or not re.fullmatch(r'executor-[a-f0-9-]{36}-1', planned['pod']) or not re.fullmatch(r'assignment-[a-f0-9]{32}', planned['config']):
                    raise ValueError('Trusted driver did not publish a bounded fixed workload plan')
                command(['create', '-f', '-'], {'apiVersion': 'v1', 'kind': 'List', 'items': [
                    {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'Role', 'metadata': {'name': 'lifecycle-controller', 'namespace': namespace}, 'rules': [
                        {'apiGroups': [''], 'resources': ['pods','configmaps'], 'verbs': ['create']},
                        {'apiGroups': [''], 'resources': ['pods'], 'resourceNames': [planned['pod']], 'verbs': ['get','delete']},
                        {'apiGroups': [''], 'resources': ['configmaps'], 'resourceNames': [planned['config']], 'verbs': ['get','delete']},
                        {'apiGroups': [''], 'resources': ['pods/exec'], 'resourceNames': [planned['pod']], 'verbs': ['get','create']}]},
                    {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'RoleBinding', 'metadata': {'name': 'lifecycle-controller', 'namespace': namespace},
                     'roleRef': {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Role', 'name': 'lifecycle-controller'},
                     'subjects': [{'kind': 'ServiceAccount', 'name': 'lifecycle-controller', 'namespace': namespace}]}]})
                private(ready, b'ready\n')
                if process.wait(timeout=150):
                    log.seek(0)
                    raise ValueError('Composed lifecycle fixture failed: '+log.read(4096).decode(errors='replace'))
            with output.open('rb') as stream:
                raw = stream.read(32*1024*1024+1)
            if len(raw)>32*1024*1024:
                raise ValueError('Candidate bound')
            expected = dict(bundle, files=list(bundle['files'])+[{'path': FIXTURE_PATH, 'mode': 420, 'data': base64.b64encode(FIXTURE_BODY).decode()}])
            if identities(json.loads(raw))!=identities(expected):
                raise ValueError('External candidate identity comparison failed')
            result = json.loads(report.read_bytes())
            result.update(sourceSHA=bundle['sourceSHA'], sourceFiles=len(bundle['files']), sourceTreeSHA256=tree_digest(bundle),
                          candidateIndependentlyCompared=True, resourcesConfirmedRemoved=True)
    finally:
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        if namespace_uid:
            network_probe.cleanup_namespace(command, namespace, namespace_uid)
    result.update(temporaryNamespaceRemoved=True, durationSeconds=round(time.monotonic()-started, 3))
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('kubectl','kubeconfig','worker-image','mailbox-image','test-binary','source-repository','source-sha'):
        parser.add_argument('--'+name, required=True)
    args = parser.parse_args()
    print(json.dumps(verify([args.kubectl,'--kubeconfig',args.kubeconfig], args.worker_image, args.mailbox_image,
        args.test_binary, source_bundle(Path(args.source_repository),args.source_sha)), indent=2))


if __name__ == '__main__':
    main()
