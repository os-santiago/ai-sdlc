#!/usr/bin/env python3
"""Prove a repaired candidate through real behavior/review controllers and handoff.

Initial generation history and model replies are synthetic. Both behavior and
independent review passes use actual controllers. Source is a pinned
Git snapshot. This disposable fixture never writes to a forge or installed work.
"""
import argparse
from contextlib import contextmanager, ExitStack
import hashlib
import json
import os
import secrets
from pathlib import Path
import subprocess
import tempfile

from network_probe import cleanup_namespace
from verify_candidate_runtime import verify as verify_candidate
from verify_pod_transport import TransportFixture
from verify_workspace_bundle import source_bundle, wire


@contextmanager
def controller(kubectl, base, name, execute):
    namespace = 'ai-sdlc-' + name
    uid = None
    nonce = secrets.token_hex(16)
    def raw_command(args, value=None, timeout=65):
        result = subprocess.run(kubectl + args, input=wire(value) if isinstance(value, dict) else value,
                                capture_output=True, timeout=timeout)
        if result.returncode:
            raise ValueError('Owned repair composition Kubernetes operation failed')
        return result.stdout
    def guard():
        observed = json.loads(raw_command(['get', 'namespace', namespace, '-o', 'json']))
        metadata = observed.get('metadata', {})
        if (metadata.get('uid') != uid or metadata.get('deletionTimestamp')
                or metadata.get('labels', {}).get('ai-sdlc.io/repair-fixture') != nonce):
            raise ValueError('Original repair fixture namespace changed')
    def command(args, value=None, timeout=65):
        if uid:
            guard()
        result = raw_command(args, value, timeout)
        if uid:
            guard()
        return result
    try:
        created = json.loads(command(['create', '-f', '-', '-o', 'json'], {
            'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
                'labels': {'pod-security.kubernetes.io/enforce': 'restricted', 'ai-sdlc.io/repair-fixture': nonce}}}))
        uid = created['metadata']['uid']
        guard()
        rules = [
            {'apiGroups': [''], 'resources': ['pods', 'configmaps'], 'verbs': ['get', 'create', 'delete']},
            {'apiGroups': ['networking.k8s.io'], 'resources': ['networkpolicies'], 'verbs': ['list']},
            {'apiGroups': [''], 'resources': ['pods/exec' if execute else 'pods/log'], 'verbs': ['get', 'create'] if execute else ['get']}]
        command(['create', '-f', '-'], {'apiVersion': 'v1', 'kind': 'List', 'items': [
            {'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy', 'metadata': {'name': 'deny-all', 'namespace': namespace},
             'spec': {'podSelector': {}, 'policyTypes': ['Ingress', 'Egress']}},
            {'apiVersion': 'v1', 'kind': 'ServiceAccount', 'metadata': {'name': name, 'namespace': namespace}, 'automountServiceAccountToken': False},
            {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'Role', 'metadata': {'name': name, 'namespace': namespace}, 'rules': rules},
            {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'RoleBinding', 'metadata': {'name': name, 'namespace': namespace},
             'roleRef': {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Role', 'name': name},
             'subjects': [{'kind': 'ServiceAccount', 'name': name, 'namespace': namespace}]}]})
        token = command(['create', 'token', name, '-n', namespace, '--duration=10m']).decode().strip()
        guard()
        yield dict(APIServer=base['APIServer'], CA=base['CA'], BearerToken=token)
        guard()
        for kind in ('pods', 'configmaps'):
            observed = json.loads(command(['get', kind, '-n', namespace, '-o', 'json']))['items']
            if any(item['metadata']['name'] != 'kube-root-ca.crt' for item in observed):
                raise ValueError('Repair composition controller resources were not retired')
    finally:
        if uid:
            cleanup_namespace(raw_command, namespace, uid)


def verify(kubectl, worker, mailbox, binary, verifier_image, repository, source_sha, candidate_directory):
    binary = Path(binary).resolve()
    bundle = source_bundle(repository, source_sha)
    changes = {}
    for name in ('quarkus-app/src/main/resources/META-INF/resources/js/utils.js', 'tests/js/utils-locale-fallback.test.js'):
        raw = (Path(candidate_directory) / name).read_bytes()
        if not 1 <= len(raw) <= 32768:
            raise ValueError('Bounded predecessor candidate required')
        changes[name] = raw.decode('utf-8')
    base = TransportFixture(kubectl, binary, False).config
    with ExitStack() as stack:
        verifier = stack.enter_context(controller(kubectl, base, 'verifiers', False))
        reviewer = stack.enter_context(controller(kubectl, base, 'reviewers', True))
        directory = Path(stack.enter_context(tempfile.TemporaryDirectory(prefix='ai-sdlc-repair-source-')))
        source = directory / 'source.json'
        fd = os.open(source, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, 'wb') as stream:
            stream.write(wire(bundle))
        result = verify_candidate(kubectl, worker, mailbox, str(binary), source_sha, 'repair-composition',
            dict(SourceFile=str(source), Changes=changes, VerificationConfig=verifier,
                 ReviewConfig=reviewer, VerificationImage=verifier_image))
    root = Path(__file__).resolve().parents[1]
    def git(*args):
        return subprocess.run(['git', '-C', str(root), *args], check=True, capture_output=True).stdout
    return dict(result, verifierNamespaceRemoved=True, reviewerNamespaceRemoved=True,
                separatedControllerIdentities=True, binarySHA256=hashlib.sha256(binary.read_bytes()).hexdigest(),
                sourceRevision=source_sha, runtimeRevision=git('rev-parse', 'HEAD').decode().strip(),
                runtimeDirty=bool(git('status', '--porcelain')), runtimeDiffSHA256=hashlib.sha256(git('diff', 'HEAD')).hexdigest(),
                workerImage=worker, mailboxImage=mailbox, verifierImage=verifier_image)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('kubectl', 'kubeconfig', 'worker-image', 'mailbox-image', 'test-binary', 'verifier-image', 'repository', 'source-sha', 'candidate-directory'):
        parser.add_argument('--' + name, required=True)
    parser.add_argument('--report', type=Path, required=True)
    args = parser.parse_args()
    result = verify([args.kubectl, '--kubeconfig', args.kubeconfig], args.worker_image, args.mailbox_image,
                    args.test_binary, args.verifier_image, args.repository, args.source_sha, args.candidate_directory)
    args.report.write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result, indent=2))


if __name__ == '__main__':
    main()
