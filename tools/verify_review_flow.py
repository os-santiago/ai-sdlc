#!/usr/bin/env python3
"""Compose isolated generation, behavior verification and independent review."""
import argparse
from contextlib import contextmanager
import secrets
import signal
import socket
import sys
import time
import urllib.request
import base64
import json
import os
from pathlib import Path
import subprocess
import tempfile

from network_probe import cleanup_namespace
from verify_pod_transport import TransportFixture
from verify_verification_flow import verify as verify_behavior
from verify_workspace_bundle import wire


@contextmanager
def owned_provider(scenario):
    if scenario != 'real-model':
        yield {}
        return
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    token = secrets.token_urlsafe(48)
    wrapper = Path(__file__).resolve().parents[1] / 'runtime/k8s/tools/codexcli_openai_wrapper.py'
    env = dict(os.environ, CODEX_WRAPPER_PORT=str(port), CODEX_WRAPPER_TOKEN=token)
    process = subprocess.Popen([sys.executable, str(wrapper)], env=env,
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        deadline = time.monotonic() + 15
        while True:
            if process.poll() is not None:
                raise RuntimeError('Owned completion adapter failed startup')
            try:
                with urllib.request.urlopen(f'http://127.0.0.1:{port}/healthz', timeout=1) as response:
                    if response.status == 200:
                        break
            except OSError:
                if time.monotonic() >= deadline:
                    raise RuntimeError('Owned completion adapter readiness deadline') from None
                time.sleep(.1)
        yield dict(Endpoint=f'http://127.0.0.1:{port}/v1/chat/completions', Token=token)
    finally:
        process.send_signal(signal.SIGINT)
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
            raise RuntimeError('Owned completion adapter required forced termination') from None


def verify(kubectl, worker, mailbox, candidate_binary, review_binary, source_sha, candidate_directory, scenario):
    if scenario not in ('approve', 'request_changes', 'mutation', 'revoke-model', 'revoke-config', 'lost-config-reply', 'real-model'):
        raise ValueError('Unsupported review scenario')
    config = TransportFixture(kubectl, review_binary, False).config
    namespace, uid = 'ai-sdlc-reviewers', None

    def command(args, value=None, timeout=65):
        result = subprocess.run(kubectl + args, input=wire(value) if isinstance(value, dict) else value,
                                capture_output=True, timeout=timeout)
        if result.returncode:
            raise ValueError('Review fixture Kubernetes operation failed')
        return result.stdout

    previous = os.environ.get('AI_SDLC_REVIEW_CONTROL')
    try:
        created = json.loads(command(['create', '-f', '-', '-o', 'json'], {
            'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
            'labels': {'pod-security.kubernetes.io/enforce': 'restricted'}}}))
        uid = created['metadata']['uid']
        command(['create', '-f', '-'], {'apiVersion': 'v1', 'kind': 'List', 'items': [
            {'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy', 'metadata': {'name': 'deny-all', 'namespace': namespace},
             'spec': {'podSelector': {}, 'policyTypes': ['Ingress', 'Egress']}},
            {'apiVersion': 'v1', 'kind': 'ServiceAccount', 'metadata': {'name': 'reviewer', 'namespace': namespace}, 'automountServiceAccountToken': False},
            {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'Role', 'metadata': {'name': 'reviewer', 'namespace': namespace}, 'rules': [
                {'apiGroups': [''], 'resources': ['pods', 'configmaps'], 'verbs': ['get', 'create', 'delete']},
                {'apiGroups': [''], 'resources': ['pods/exec'], 'verbs': ['get', 'create']},
                {'apiGroups': ['networking.k8s.io'], 'resources': ['networkpolicies'], 'verbs': ['list']}]},
            {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'RoleBinding', 'metadata': {'name': 'reviewer', 'namespace': namespace},
             'roleRef': {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Role', 'name': 'reviewer'},
             'subjects': [{'kind': 'ServiceAccount', 'name': 'reviewer', 'namespace': namespace}]}]})
        token = command(['create', 'token', 'reviewer', '-n', namespace, '--duration=10m']).decode().strip()
        adev = Path(__file__).resolve().parents[1].joinpath('runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime/files/adev-profile.yaml').read_bytes()
        with owned_provider(scenario) as provider, tempfile.TemporaryDirectory(prefix='ai-sdlc-review-control-') as directory:
            control = Path(directory) / 'control.json'
            fd = os.open(control, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, 'wb') as stream:
                stream.write(wire(dict(Config=dict(APIServer=config['APIServer'], CA=config['CA'], BearerToken=token),
                                       MailboxImage=mailbox, Scenario=scenario, ADev=base64.b64encode(adev).decode(), **provider)))
            os.environ['AI_SDLC_REVIEW_CONTROL'] = str(control)
            result = verify_behavior(kubectl, worker, mailbox, candidate_binary, review_binary,
                                     source_sha, candidate_directory, 'success')
        for kind in ('pods', 'configmaps'):
            resources = json.loads(command(['-n', namespace, 'get', kind, '-o', 'json']))['items']
            remaining = [item for item in resources if item['metadata']['name'] != 'kube-root-ca.crt']
            expected = 1 if scenario == 'lost-config-reply' and kind == 'configmaps' else 0
            if len(remaining) != expected:
                raise ValueError('Reviewer resources were not retired')
    finally:
        if previous is None:
            os.environ.pop('AI_SDLC_REVIEW_CONTROL', None)
        else:
            os.environ['AI_SDLC_REVIEW_CONTROL'] = previous
        if uid:
            cleanup_namespace(command, namespace, uid)
    return dict(result, reviewerNamespaceRemoved=True, reviewerResourcesConfirmedRemoved=scenario != 'lost-config-reply',
                unknownResourcePreserved=scenario == 'lost-config-reply',
                reviewerServiceAccountSeparated=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('kubectl', 'kubeconfig', 'worker-image', 'mailbox-image', 'source-sha'):
        parser.add_argument('--' + name, required=True)
    for name in ('candidate-binary', 'review-binary', 'candidate-directory', 'report'):
        parser.add_argument('--' + name, required=True, type=Path)
    parser.add_argument('--scenario', choices=('approve', 'request_changes', 'mutation', 'revoke-model', 'revoke-config', 'lost-config-reply', 'real-model'), default='approve')
    args = parser.parse_args()
    result = verify([args.kubectl, '--kubeconfig', args.kubeconfig], args.worker_image, args.mailbox_image,
                    args.candidate_binary, args.review_binary, args.source_sha, args.candidate_directory, args.scenario)
    args.report.write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result, indent=2))


if __name__ == '__main__':
    main()
