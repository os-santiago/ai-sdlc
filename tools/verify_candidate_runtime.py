#!/usr/bin/env python3
"""Exercise the operational candidate poller in an explicitly disposable cluster."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import urllib.parse

import network_probe
from verify_pod_transport import TransportFixture
from verify_workspace_bundle import wire


def verify(kubectl, worker, mailbox, binary, source_sha, scenario="success", verification=None):
    database = urllib.parse.urlsplit(os.environ.get('AI_SDLC_TEST_DATABASE_URL', ''))
    if database.hostname not in ('127.0.0.1', 'localhost', '::1') or database.path != '/ai_sdlc_test':
        raise ValueError('Explicit disposable local ai_sdlc_test database required; it will be reset')
    namespace = 'ai-sdlc-executors'
    transport = TransportFixture(kubectl, binary)
    namespace_uid = None
    def command(args, value=None, timeout=45):
        result = subprocess.run(kubectl + args, input=wire(value) if isinstance(value, dict) else value,
                                capture_output=True, timeout=timeout)
        if result.returncode:
            raise ValueError('Candidate fixture Kubernetes operation failed')
        return result.stdout
    # Refuse adoption: this namespace must not exist, including in a live installation.
    try:
        created = json.loads(command(['create', '-f', '-', '-o', 'json'], {
            'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
            'labels': {'app.kubernetes.io/part-of': 'ai-sdlc-candidate-conformance',
                       'pod-security.kubernetes.io/enforce': 'restricted'}}}))
        namespace_uid = created['metadata']['uid']
        command(['create', '-f', '-'], {'apiVersion': 'v1', 'kind': 'List', 'items': [
            {'apiVersion': 'v1', 'kind': 'ServiceAccount', 'metadata': {'name': 'candidate-controller', 'namespace': namespace}, 'automountServiceAccountToken': False},
            {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'Role', 'metadata': {'name': 'candidate-controller', 'namespace': namespace}, 'rules': [
                {'apiGroups': [''], 'resources': ['pods', 'configmaps'], 'verbs': ['create', 'get', 'delete']},
                {'apiGroups': [''], 'resources': ['pods/exec'], 'verbs': ['get', 'create']}]},
            {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'RoleBinding', 'metadata': {'name': 'candidate-controller', 'namespace': namespace},
             'roleRef': {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Role', 'name': 'candidate-controller'},
             'subjects': [{'kind': 'ServiceAccount', 'name': 'candidate-controller', 'namespace': namespace}]},
            {'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy', 'metadata': {'name': 'deny-all', 'namespace': namespace},
             'spec': {'podSelector': {}, 'policyTypes': ['Ingress', 'Egress']}}]})
        token = command(['-n', namespace, 'create', 'token', 'candidate-controller', '--duration=10m']).decode().strip()
        adev = Path(__file__).resolve().parents[1].joinpath('runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime/files/adev-profile.yaml').read_text()
        runtime = {'version': 'ai-sdlc.candidate-runtime/v1', 'aDev': adev,
                   'profile': {'version': 'ai-sdlc.executor-profile/v1', 'model': 'codex-cli', 'maxSteps': 4, 'maxSeconds': 30 if scenario == 'crash-after-model-start' else 90, 'maxTokens': 2048, 'aDevSHA256': hashlib.sha256(adev.encode()).hexdigest()},
                   'placement': {'Namespace': namespace, 'WorkerImage': worker, 'MailboxImage': mailbox},
                   'model': {'Limits': {'Calls': 4, 'InputBytes': 524288, 'OutputBytes': 131072}, 'ResponseLimit': 32768}}
        with tempfile.TemporaryDirectory(prefix='ai-sdlc-candidate-proof-') as directory:
            root = Path(directory)
            control, report = root/'control.json', root/'report.json'
            fd = os.open(control, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, 'wb') as stream:
                stream.write(wire({'Config': {'APIServer': transport.config['APIServer'], 'CA': transport.config['CA'], 'BearerToken': token},
                                   'Runtime': runtime, 'SourceSHA': source_sha, 'ReportFile': str(report), 'Scenario': scenario, **(verification or {})}))
            (root/'go-tmp').mkdir()
            child_env = dict(os.environ, AI_SDLC_CANDIDATE_FIXTURE=str(control), TMPDIR=str(root/'go-tmp'))
            result = subprocess.run([str(Path(binary).resolve()), '-test.run', '^TestRealCandidateRuntimeFixture$', '-test.v'],
                env=child_env, capture_output=True, timeout=420 if verification else 210)
            if scenario == 'crash-after-model-start' and result.returncode == 91:
                result = subprocess.run([str(Path(binary).resolve()), '-test.run', '^TestRealCandidateRecoveryFixture$', '-test.v'], env=child_env, capture_output=True, timeout=210)
            if result.returncode:
                raise ValueError('Candidate runtime fixture failed: ' + (result.stdout + result.stderr)[-4096:].decode(errors='replace'))
            result = json.loads(report.read_bytes())
            for kind in ('pods', 'configmaps'):
                resources = json.loads(command(['-n', namespace, 'get', kind, '-o', 'json']))['items']
                if any(r['metadata']['name'] != 'kube-root-ca.crt' for r in resources):
                    raise ValueError('Executor resources were not retired')
            result['resourcesConfirmedRemoved'] = True
    finally:
        if namespace_uid:
            network_probe.cleanup_namespace(command, namespace, namespace_uid)
    result['ownedNamespaceRemoved'] = True
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('kubectl', 'kubeconfig', 'worker-image', 'mailbox-image', 'test-binary', 'source-sha'):
        parser.add_argument('--' + name, required=True)
    parser.add_argument('--scenario', choices=('success', 'crash-after-model-start'), default='success')
    args = parser.parse_args()
    print(json.dumps(verify([args.kubectl, '--kubeconfig', args.kubeconfig], args.worker_image,
        args.mailbox_image, args.test_binary, args.source_sha, args.scenario), indent=2))


if __name__ == '__main__':
    main()
