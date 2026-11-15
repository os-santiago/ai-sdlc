#!/usr/bin/env python3
"""Exercise SCC's read-only reviewer in an owned disposable kind namespace."""
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

from network_probe import cleanup_namespace
from verify_executor import tree_digest
from verify_pod_transport import TransportFixture
from verify_workspace_bundle import wire


def verify(kubectl, binary, worker, mailbox, scenario):
    if scenario not in ('approve', 'request_changes', 'implicit', 'mutation'):
        raise ValueError('Unsupported reviewer fixture')
    config = TransportFixture(kubectl, binary, False).config
    namespace, uid = 'ai-sdlc-reviewcheck-' + secrets.token_hex(6), None

    def command(args, value=None, timeout=65):
        result = subprocess.run(kubectl + args, input=wire(value) if isinstance(value, dict) else value,
                                capture_output=True, timeout=timeout)
        if result.returncode:
            raise ValueError('Reviewer fixture Kubernetes operation failed')
        return result.stdout

    try:
        created = json.loads(command(['create', '-f', '-', '-o', 'json'], {
            'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
            'labels': {'pod-security.kubernetes.io/enforce': 'restricted'}}}))
        uid = created['metadata']['uid']
        command(['create', '-f', '-'], {'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy',
                'metadata': {'name': 'deny-all', 'namespace': namespace},
                'spec': {'podSelector': {}, 'policyTypes': ['Ingress', 'Egress']}})
        bundle = {'version': 'ai-sdlc.workspace-bundle/v1', 'sourceSHA': 'a' * 40,
                  'files': [{'path': 'REVIEW_FIXTURE.txt', 'mode': 420,
                             'data': base64.b64encode(b'Read-only review fixture.\n').decode()}]}
        adev = Path(__file__).resolve().parents[1].joinpath(
            'runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime/files/adev-profile.yaml').read_bytes()
        profile = wire({'version': 'ai-sdlc.reviewer-profile/v1', 'model': 'fixture-model',
                        'maxSteps': 4, 'maxSeconds': 90, 'maxTokens': 2048,
                        'aDevSHA256': hashlib.sha256(adev).hexdigest()})
        assignment = wire({'version': 'ai-sdlc.executor-assignment/v1', 'workItemId': str(uuid.uuid4()),
                          'attempt': 1, 'repository': 'fixture/product', 'sourceSHA': bundle['sourceSHA'],
                          'treeSHA256': tree_digest(bundle), 'profileSHA256': hashlib.sha256(profile).hexdigest(),
                          'authorityRevision': hashlib.sha256(b'disposable review fixture').hexdigest(),
                          'issueNumbers': [1], 'title': 'Read-only reviewer conformance',
                          'body': 'Inspect REVIEW_FIXTURE.txt without changing any source.',
                          'acceptanceCriteria': ['Preserve the original source exactly.'],
                          'expiresAt': int((time.time() + 300) * 1000)})
        with tempfile.TemporaryDirectory(prefix='ai-sdlc-reviewer-') as directory:
            root = Path(directory)
            (root / 'source.json').write_bytes(wire(bundle))
            payload = dict(Config=config, Binding=dict(Namespace=namespace, WorkerImage=worker, MailboxImage=mailbox,
                           Assignment=base64.b64encode(assignment).decode(), Profile=base64.b64encode(profile).decode(),
                           ADev=base64.b64encode(adev).decode()), SourceFile=str(root/'source.json'),
                           ReportFile=str(root/'report.json'), Scenario=scenario)
            fd = os.open(root/'control.json', os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, 'wb') as stream:
                stream.write(wire(payload))
            env = dict(os.environ, AI_SDLC_REVIEWER_FIXTURE=str(root/'control.json'))
            result = subprocess.run([str(binary.resolve()), '-test.run', '^TestRealReadonlyReviewerFixture$', '-test.v'],
                                    env=env, capture_output=True, timeout=190)
            if result.returncode:
                raise ValueError('Readonly reviewer fixture failed: ' + result.stdout[-4096:].decode(errors='replace'))
            report = json.loads((root/'report.json').read_bytes())
        for kind in ('pods', 'configmaps'):
            objects = json.loads(command(['-n', namespace, 'get', kind, '-o', 'json']))['items']
            if any(item['metadata']['name'] != 'kube-root-ca.crt' for item in objects):
                raise ValueError('Reviewer resources were not retired')
    finally:
        if uid:
            cleanup_namespace(command, namespace, uid)
    return dict(report, ownedNamespaceRemoved=True, resourcesConfirmedRemoved=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('kubectl', 'kubeconfig', 'worker-image', 'mailbox-image'):
        parser.add_argument('--' + name, required=True)
    parser.add_argument('--test-binary', type=Path, required=True)
    parser.add_argument('--scenario', choices=('approve', 'request_changes', 'implicit', 'mutation'), default='approve')
    parser.add_argument('--report', type=Path, required=True)
    args = parser.parse_args()
    report = verify([args.kubectl, '--kubeconfig', args.kubeconfig], args.test_binary,
                    args.worker_image, args.mailbox_image, args.scenario)
    args.report.write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    main()
