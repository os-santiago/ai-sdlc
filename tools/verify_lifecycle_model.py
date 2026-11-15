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


def verify(kubectl, worker_image, mailbox_image, test_binary, bundle, scenario="success", model_connection=None):
    if scenario not in ("success", "three-turns", "provider-failure", "cancel-provider", "no-change", "real-model"):
        raise ValueError("Unknown controller scenario")
    if scenario == "real-model":
        if not isinstance(model_connection, dict) or set(model_connection) != {"Endpoint", "Token"}:
            raise ValueError("Owned loopback model connection required")
        endpoint = urllib.parse.urlsplit(model_connection["Endpoint"])
        if (endpoint.scheme != "http" or endpoint.hostname != "127.0.0.1" or not endpoint.port
                or endpoint.path != "/v1/chat/completions" or endpoint.query or endpoint.fragment
                or endpoint.username or endpoint.password or not re.fullmatch(r"[A-Za-z0-9_-]{32,128}", model_connection["Token"])):
            raise ValueError("Invalid fixture model connection")
    elif model_connection is not None:
        raise ValueError("Synthetic scenarios cannot accept a model connection")
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
    profile = wire({'version': 'ai-sdlc.executor-profile/v1', 'model': 'codex-cli' if scenario == 'real-model' else 'fixture-model',
                    'maxSteps': 24 if scenario == 'real-model' else 4, 'maxSeconds': 180 if scenario == 'real-model' else 45, 'maxTokens': 2048,
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
            delta_file = root/'delta.json'
            plan, ready, control = root/'plan.json', root/'ready', root/'control.json'
            private(source, wire(bundle))
            private(control, wire({'Config': config, 'Placement': {'Namespace': namespace,
                'WorkerImage': worker_image, 'MailboxImage': mailbox_image},
                'Profile': base64.b64encode(profile).decode(), 'ADev': base64.b64encode(adev).decode(),
                'SourceFile': str(source), 'OutputFile': str(output), 'ReportFile': str(report),
                'PlanFile': str(plan), 'ReadyFile': str(ready), 'DeltaFile': str(delta_file), 'Scenario': scenario, **(model_connection or {})}))
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
                if process.wait(timeout=270):
                    log.seek(0)
                    raise ValueError('Composed lifecycle fixture failed: '+log.read(4096).decode(errors='replace'))
            result = json.loads(report.read_bytes())
            for kind, name in (("pod", planned["pod"]), ("configmap", planned["config"])):
                if command(["-n", namespace, "get", kind, name, "--ignore-not-found", "-o", "json"]).strip():
                    raise ValueError("Controller did not retire its acknowledged resources")
            if scenario in ("provider-failure", "cancel-provider", "no-change"):
                if result.get("claimStopped") is not True or result.get("candidateCaptured") is not False or result.get("modelCalls") not in (range(1, 5) if scenario == "no-change" else (1,)) or output.exists() or delta_file.exists():
                    raise ValueError("Failed controller exposed candidate or retained authority")
                if scenario == "no-change" and result.get("workerNonzeroExitObserved") is not True:
                    raise ValueError("No-change scenario did not observe the original worker rejection")
                result.update(resourcesConfirmedRemoved=True)
            else:
                with output.open('rb') as stream:
                    raw = stream.read(32*1024*1024+1)
                if len(raw)>32*1024*1024:
                    raise ValueError('Candidate bound')
                expected = dict(bundle, files=list(bundle['files'])+[{'path': FIXTURE_PATH, 'mode': 420, 'data': base64.b64encode(FIXTURE_BODY).decode()}])
                if identities(json.loads(raw))!=identities(expected):
                    raise ValueError('External candidate identity comparison failed')
                with delta_file.open('rb') as stream:
                    delta_raw = stream.read(2*1024*1024+1)
                if len(delta_raw)>2*1024*1024 or hashlib.sha256(delta_raw).hexdigest()!=result['candidateDeltaSHA256']:
                    raise ValueError('Persisted delta bound/digest mismatch')
                delta = json.loads(delta_raw)
                if (delta['version']!='ai-sdlc.candidate-delta/v1' or delta['sourceSHA']!=bundle['sourceSHA']
                        or delta['sourceTreeSHA256']!=tree_digest(bundle) or delta['candidateTreeSHA256']!=tree_digest(expected)):
                    raise ValueError('Persisted delta tree identity mismatch')
                # This fixture has exactly one externally prescribed new file.
                # Compare the entire persisted change against that independent value.
                expected_change = {'path':FIXTURE_PATH,'before':None,
                                   'after':{'path':FIXTURE_PATH,'mode':420,'data':base64.b64encode(FIXTURE_BODY).decode()}}
                if delta['changes']!=[expected_change] or result['candidateValidated'] is not False:
                    raise ValueError('Persisted candidate is not the prescribed unvalidated delta')
                reconstructed = dict(bundle,files=list(bundle['files'])+[delta['changes'][0]['after']])
                if identities(reconstructed)!=identities(json.loads(raw)):
                    raise ValueError('External persisted delta reconstruction differs from export')
                result.update(sourceSHA=bundle['sourceSHA'], sourceFiles=len(bundle['files']), sourceTreeSHA256=tree_digest(bundle),
                              candidateIndependentlyCompared=True, persistedDeltaIndependentlyReconstructed=True, resourcesConfirmedRemoved=True)
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
    parser.add_argument("--scenario", choices=("success", "three-turns", "provider-failure", "cancel-provider", "no-change"), default="success")
    args = parser.parse_args()
    print(json.dumps(verify([args.kubectl,'--kubeconfig',args.kubeconfig], args.worker_image, args.mailbox_image,
        args.test_binary, source_bundle(Path(args.source_repository),args.source_sha), args.scenario), indent=2))


if __name__ == '__main__':
    main()
