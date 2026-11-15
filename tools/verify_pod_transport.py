#!/usr/bin/env python3
"""Compose real SCC/gVisor, Kubernetes helpers and PostgreSQL with synthetic model replies."""
import argparse
import base64
import json
import os
from pathlib import Path
import subprocess
import tempfile

from verify_executor import verify
from verify_workspace_bundle import source_bundle, wire


class TransportFixture:
    def __init__(self, kubectl, test_binary):
        self.kubectl = kubectl
        self.test_binary = str(Path(test_binary).resolve())
        # Capture only into memory. Never print kubeconfig, keys or tokens.
        result = subprocess.run(kubectl + ['config', 'view', '--raw', '--minify', '-o', 'json'],
                                capture_output=True, timeout=10)
        if result.returncode:
            raise ValueError('Cannot read isolated fixture kubeconfig')
        config = json.loads(result.stdout)
        cluster, user = config['clusters'][0]['cluster'], config['users'][0]['user']
        self.config = {'APIServer': cluster['server'], 'CA': cluster['certificate-authority-data'],
                       'ClientCertificate': user['client-certificate-data'], 'ClientKey': user['client-key-data']}
        self.database = os.environ.get('AI_SDLC_TEST_DATABASE_URL')
        if not self.database:
            raise ValueError('Disposable PostgreSQL fixture URL is required in AI_SDLC_TEST_DATABASE_URL')
        self.binding = None

    def run(self, scenario, config=None, replacement_uid=None):
        payload = {'Config': config or self.config, 'Binding': self.binding,
                   'Scenario': scenario, 'DatabaseURL': self.database, 'ReplacementUID': replacement_uid or ''}
        # tempfile is 0600; its enclosing directory is private and removed even
        # on failure. The Go fixture emits only fixed, sanitized diagnostics.
        with tempfile.TemporaryDirectory(prefix='ai-sdlc-transport-') as directory:
            path = Path(directory) / 'input.json'
            descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(descriptor, 'wb') as stream:
                stream.write(wire(payload))
            env = dict(os.environ, AI_SDLC_POD_TRANSPORT_FIXTURE=str(path))
            result = subprocess.run([self.test_binary, '-test.run', '^TestRealKubernetesMailboxFixture$', '-test.v'],
                                    env=env, capture_output=True, timeout=55)
            if result.returncode:
                raise ValueError('Actual transport fixture failed (' + scenario + '): '
                                 + result.stdout.decode(errors='replace')[:2000])

    def exchange(self, workload, pod_uid, assignment, profile, adev, command):
        namespace, name = workload['pod']['metadata']['namespace'], workload['pod']['metadata']['name']
        images = {item['name']: item['image'] for item in workload['pod']['spec']['containers']}
        self.binding = {'Namespace': namespace, 'WorkerImage': images['worker'], 'MailboxImage': images['mailbox'],
                        'PodUID': pod_uid, 'Assignment': base64.b64encode(assignment).decode(),
                        'Profile': base64.b64encode(profile).decode(), 'ADev': base64.b64encode(adev).decode()}
        command(['create', '-f', '-'], {'apiVersion': 'v1', 'kind': 'List', 'items': [
            {'apiVersion': 'v1', 'kind': 'ServiceAccount', 'metadata': {'name': 'observer', 'namespace': namespace},
             'automountServiceAccountToken': False},
            {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'Role',
             'metadata': {'name': 'observe-only', 'namespace': namespace},
             'rules': [{'apiGroups': [''], 'resources': ['pods'], 'resourceNames': [name], 'verbs': ['get']}]},
            {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'RoleBinding',
             'metadata': {'name': 'observe-only', 'namespace': namespace},
             'roleRef': {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Role', 'name': 'observe-only'},
             'subjects': [{'kind': 'ServiceAccount', 'name': 'observer', 'namespace': namespace}]}]})
        token = command(['-n', namespace, 'create', 'token', 'observer', '--duration=10m']).decode().strip()
        restricted = {'APIServer': self.config['APIServer'], 'CA': self.config['CA'], 'BearerToken': token}
        self.run('permission-denied', restricted)
        self.run('normal')
        return {1, 2}  # Go independently requires these two completed durable handoffs.

    def verify_replacement(self, workload, original_uid, command):
        namespace, name = workload['pod']['metadata']['namespace'], workload['pod']['metadata']['name']
        observed = json.loads(command(['-n', namespace, 'get', 'pod', name, '-o', 'json']))
        if observed['metadata']['uid'] != original_uid:
            raise ValueError('Fixture Pod changed before replacement test')
        # This disposable namespace is owned by the fixture; no live work is advanced.
        command(['-n', namespace, 'delete', 'pod', name, '--wait=true', '--timeout=30s'])
        replacement = json.loads(command(['create', '-f', '-', '-o', 'json'], workload['pod']))
        uid = replacement['metadata']['uid']
        if uid == original_uid:
            raise ValueError('Kubernetes did not create a new Pod identity')
        command(['-n', namespace, 'wait', '--for=condition=Ready', 'pod/' + name, '--timeout=60s'], timeout=70)
        self.run('replaced', replacement_uid=uid)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('kubectl', 'kubeconfig', 'render-image', 'mailbox-image', 'worker-image', 'test-binary', 'source-repository', 'source-sha'):
        parser.add_argument('--' + name, required=True)
    parser.add_argument('--provider', choices=('docker', 'podman'), required=True)
    args = parser.parse_args()
    kubectl = [args.kubectl, '--kubeconfig', args.kubeconfig]
    transport = TransportFixture(kubectl, args.test_binary)
    report = verify(kubectl, args.provider, args.render_image, args.mailbox_image, args.worker_image,
                    source_bundle(Path(args.source_repository), args.source_sha), transport=transport)
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    main()
