#!/usr/bin/env python3
"""Exercise fixed Kubernetes creates, SCC helpers and UID-bound cleanup in kind."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import time
import uuid

import network_probe
from verify_executor import FIXTURE_BODY, FIXTURE_PATH, tree_digest
from verify_pod_transport import TransportFixture
from verify_workspace_bundle import identities, source_bundle, wire


def verify(kubectl, worker_image, mailbox_image, test_binary, bundle):
    started = time.monotonic()
    namespace = 'ai-sdlc-resourcecheck-' + secrets.token_hex(6)
    namespace_uid = None
    if FIXTURE_PATH in identities(bundle):
        raise ValueError('Source already contains fixture file')
    adev = Path(__file__).resolve().parents[1].joinpath(
        'runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime/files/adev-profile.yaml').read_bytes()
    profile = wire({'version': 'ai-sdlc.executor-profile/v1', 'model': 'fixture-model',
                    'maxSteps': 4, 'maxSeconds': 45, 'maxTokens': 2048,
                    'aDevSHA256': hashlib.sha256(adev).hexdigest()})
    assignment = {'version': 'ai-sdlc.executor-assignment/v1', 'workItemId': str(uuid.uuid4()),
                  'attempt': 1, 'repository': 'os-santiago/homedir', 'sourceSHA': bundle['sourceSHA'],
                  'treeSHA256': tree_digest(bundle), 'profileSHA256': hashlib.sha256(profile).hexdigest(),
                  'authorityRevision': hashlib.sha256(b'isolated synthetic resource fixture').hexdigest(),
                  'issueNumbers': [1], 'title': 'Isolated resource conformance fixture',
                  'body': 'Synthetic component test, not a real issue. Create ' + FIXTURE_PATH,
                  'acceptanceCriteria': ['Create ' + FIXTURE_PATH + ' containing exactly: ' + FIXTURE_BODY.decode(),
                                         'Preserve every original source file.'],
                  'expiresAt': int((time.time() + 300) * 1000)}
    assignment_raw = wire(assignment)
    pod_name = 'executor-' + assignment['workItemId'] + '-1'
    config_name = 'assignment-' + hashlib.sha256(assignment_raw).hexdigest()[:32]
    transport = TransportFixture(kubectl, test_binary, database_required=False)
    binding = {'Namespace': namespace, 'WorkerImage': worker_image, 'MailboxImage': mailbox_image,
               'PodUID': '', 'Assignment': base64.b64encode(assignment_raw).decode(),
               'Profile': base64.b64encode(profile).decode(), 'ADev': base64.b64encode(adev).decode()}

    def command(args, value=None, expected=0, timeout=45):
        result = subprocess.run(kubectl + args, input=wire(value) if isinstance(value, dict) else value,
                                capture_output=True, timeout=timeout)
        if result.returncode != expected:
            raise ValueError('Resource fixture Kubernetes bootstrap failed')
        return result.stdout

    try:
        created = json.loads(command(['create', '-f', '-', '-o', 'json'], {
            'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
            'labels': {'app.kubernetes.io/part-of': 'ai-sdlc-resource-conformance',
                       'pod-security.kubernetes.io/enforce': 'restricted'}}}))
        namespace_uid = created['metadata']['uid']
        command(['create', '-f', '-'], {'apiVersion': 'v1', 'kind': 'ServiceAccount',
                'metadata': {'name': 'resource-controller', 'namespace': namespace},
                'automountServiceAccountToken': False})
        token = command(['-n', namespace, 'create', 'token', 'resource-controller', '--duration=10m']).decode().strip()
        config = {'APIServer': transport.config['APIServer'], 'CA': transport.config['CA'], 'BearerToken': token}
        with tempfile.TemporaryDirectory(prefix='ai-sdlc-resource-fixture-') as directory:
            root = Path(directory)
            source, output, report = root/'source.json', root/'candidate.json', root/'report.json'
            def private(path, raw):
                fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                with os.fdopen(fd, 'wb') as stream:
                    stream.write(raw)
            private(source, wire(bundle))
            def run(scenario):
                control = root/(scenario + '.json')
                private(control, wire({'Config': config, 'Binding': binding, 'Scenario': scenario,
                                      'SourceFile': str(source), 'OutputFile': str(output), 'ReportFile': str(report)}))
                result = subprocess.run([str(Path(test_binary).resolve()), '-test.run', '^TestRealWorkloadResourcesFixture$', '-test.v'],
                                        env=dict(os.environ, AI_SDLC_RESOURCE_FIXTURE=str(control)),
                                        capture_output=True, timeout=150)
                if result.returncode:
                    raise ValueError('Actual fixed resource fixture failed: ' + result.stdout.decode(errors='replace')[:2000])
            run('create-denied')
            command(['create', '-f', '-'], {'apiVersion': 'v1', 'kind': 'List', 'items': [
                {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'Role',
                 'metadata': {'name': 'resource-controller', 'namespace': namespace}, 'rules': [
                    {'apiGroups': [''], 'resources': ['pods', 'configmaps'], 'verbs': ['create']},
                    {'apiGroups': [''], 'resources': ['pods'], 'resourceNames': [pod_name], 'verbs': ['get', 'delete']},
                    {'apiGroups': [''], 'resources': ['configmaps'], 'resourceNames': [config_name], 'verbs': ['get', 'delete']},
                    {'apiGroups': [''], 'resources': ['pods/exec'], 'resourceNames': [pod_name], 'verbs': ['get', 'create']}]},
                {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'RoleBinding',
                 'metadata': {'name': 'resource-controller', 'namespace': namespace},
                 'roleRef': {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Role', 'name': 'resource-controller'},
                 'subjects': [{'kind': 'ServiceAccount', 'name': 'resource-controller', 'namespace': namespace}]}]})
            run('normal')
            with output.open('rb') as stream:
                raw = stream.read(32*1024*1024+1)
            if len(raw) > 32*1024*1024:
                raise ValueError('Candidate output exceeded bound')
            expected = dict(bundle, files=list(bundle['files']) + [{'path': FIXTURE_PATH, 'mode': 420,
                            'data': base64.b64encode(FIXTURE_BODY).decode()}])
            if identities(json.loads(raw)) != identities(expected):
                raise ValueError('External candidate identity comparison failed')
            result = json.loads(report.read_bytes())
            result.update(actualCreateRBACDenial=True, sourceSHA=bundle['sourceSHA'], sourceFiles=len(bundle['files']),
                          sourceTreeSHA256=assignment['treeSHA256'], candidateIndependentlyCompared=True)
    finally:
        if namespace_uid:
            network_probe.cleanup_namespace(command, namespace, namespace_uid)
    result.update(temporaryNamespaceRemoved=True, durationSeconds=round(time.monotonic()-started, 3))
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('kubectl', 'kubeconfig', 'worker-image', 'mailbox-image', 'test-binary', 'source-repository', 'source-sha'):
        parser.add_argument('--'+name, required=True)
    args = parser.parse_args()
    print(json.dumps(verify([args.kubectl, '--kubeconfig', args.kubeconfig], args.worker_image, args.mailbox_image,
                           args.test_binary, source_bundle(Path(args.source_repository), args.source_sha)), indent=2))


if __name__ == '__main__':
    main()
