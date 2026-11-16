#!/usr/bin/env python3
"""Run the fixed verification recipe through real, isolated Kubernetes transport.

Requires the installer's conformance-tested ai-sdlc-offline RuntimeClass. This
fixture consumes explicitly supplied candidate bytes, never advances live work,
and refuses an existing verifier namespace.
"""
import argparse
import base64
import json
import os
from pathlib import Path
import subprocess
import tempfile

from network_probe import cleanup_namespace
from verify_pod_transport import TransportFixture
from verify_workspace_bundle import source_bundle, wire


def verify(kubectl, binary, repository, revision, candidate, image, project="os-santiago/homedir"):
    source = source_bundle(repository, revision)
    paths = ('quarkus-app/src/main/resources/META-INF/resources/js/utils.js',
             'tests/js/utils-locale-fallback.test.js')
    if project == "os-santiago/saferus":
        paths = ("web/static/ttl.js", "tests/ttl.test.mjs", "tools/verify_browser.py")
    elif project != "os-santiago/homedir":
        raise ValueError("Unsupported reviewed fixture project")
    changes = {}
    for name in paths:
        raw = (candidate / name).read_bytes()
        if not raw or len(raw) > 32768:
            raise ValueError('Candidate fixture file exceeds recipe bounds')
        changes[name] = base64.b64encode(raw).decode()
    config = TransportFixture(kubectl, binary, False).config
    namespace, uid = 'ai-sdlc-verifiers', None

    def command(args, value=None, timeout=45):
        result = subprocess.run(kubectl + args,
                                input=wire(value) if isinstance(value, dict) else value,
                                capture_output=True, timeout=timeout)
        if result.returncode:
            raise ValueError('Isolated verifier Kubernetes operation failed')
        return result.stdout

    try:
        created = json.loads(command(['create', '-f', '-', '-o', 'json'], {
            'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
            'labels': {'pod-security.kubernetes.io/enforce': 'restricted',
                       'app.kubernetes.io/part-of': 'ai-sdlc-verifier-conformance'}}}))
        uid = created['metadata']['uid']
        command(['create', '-f', '-'], {'apiVersion': 'v1', 'kind': 'List', 'items': [
            {'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy',
             'metadata': {'name': 'deny-all', 'namespace': namespace},
             'spec': {'podSelector': {}, 'policyTypes': ['Ingress', 'Egress']}},
            {'apiVersion': 'v1', 'kind': 'ServiceAccount',
             'metadata': {'name': 'verifier', 'namespace': namespace},
             'automountServiceAccountToken': False},
            {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'Role',
             'metadata': {'name': 'verifier', 'namespace': namespace}, 'rules': [
                 {'apiGroups': [''], 'resources': ['pods', 'configmaps'], 'verbs': ['get', 'create', 'delete']},
                 {'apiGroups': [''], 'resources': ['pods/log'], 'verbs': ['get']},
                 {'apiGroups': ['networking.k8s.io'], 'resources': ['networkpolicies'], 'verbs': ['list']}]},
            {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'RoleBinding',
             'metadata': {'name': 'verifier', 'namespace': namespace},
             'roleRef': {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Role', 'name': 'verifier'},
             'subjects': [{'kind': 'ServiceAccount', 'name': 'verifier', 'namespace': namespace}]}]})
        token = command(['create', 'token', 'verifier', '-n', namespace, '--duration=10m']).decode().strip()
        restricted = {'APIServer': config['APIServer'], 'CA': config['CA'], 'BearerToken': token}
        with tempfile.TemporaryDirectory(prefix='ai-sdlc-verifier-fixture-') as directory:
            root = Path(directory)
            (root / 'source.json').write_bytes(wire(source))
            control = {'Config': restricted, 'SourceFile': str(root / 'source.json'),
                       'ReportFile': str(root / 'report.json'), 'Image': image, 'Changes': changes, 'Repository': project}
            fd = os.open(root / 'control.json', os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, 'wb') as stream:
                stream.write(wire(control))
            result = subprocess.run([str(binary.resolve()), '-test.run', '^TestRealVerificationTransportFixture$', '-test.v'],
                                    env=dict(os.environ, AI_SDLC_VERIFICATION_TRANSPORT_FIXTURE=str(root / 'control.json')),
                                    capture_output=True, timeout=140)
            if result.returncode:
                raise ValueError('Real verification transport fixture failed: ' + result.stdout.decode(errors='replace')[-2000:])
            report = json.loads((root / 'report.json').read_bytes())
    finally:
        if uid:
            cleanup_namespace(command, namespace, uid)
    return dict(report, ownedNamespaceRemoved=True, restrictedServiceAccount=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--kubectl', required=True)
    parser.add_argument('--kubeconfig', required=True)
    parser.add_argument('--test-binary', required=True, type=Path)
    parser.add_argument('--source-repository', required=True, type=Path)
    parser.add_argument('--source-sha', required=True)
    parser.add_argument('--candidate-directory', required=True, type=Path)
    parser.add_argument('--image', required=True)
    parser.add_argument('--report', required=True, type=Path)
    parser.add_argument('--project', choices=['os-santiago/homedir', 'os-santiago/saferus'], default='os-santiago/homedir')
    args = parser.parse_args()
    result = verify([args.kubectl, '--kubeconfig', args.kubeconfig], args.test_binary,
                    args.source_repository, args.source_sha, args.candidate_directory, args.image, args.project)
    args.report.write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result, indent=2))


if __name__ == '__main__':
    main()
