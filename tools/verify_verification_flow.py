#!/usr/bin/env python3
"""Prove signed candidate-to-verifier composition in disposable kind/Postgres."""
import argparse
import json
from pathlib import Path
import subprocess

from network_probe import cleanup_namespace
from verify_candidate_runtime import verify as candidate_verify
from verify_pod_transport import TransportFixture
from verify_workspace_bundle import wire


def verify(kubectl, worker, mailbox, candidate_binary, verifier_binary, source_sha, candidate_directory, scenario="success"):
    changes = {}
    for name in ('quarkus-app/src/main/resources/META-INF/resources/js/utils.js', 'tests/js/utils-locale-fallback.test.js'):
        raw = (candidate_directory / name).read_bytes()
        if not 1 <= len(raw) <= 32768:
            raise ValueError('Bounded fixture candidate required')
        changes[name] = raw.decode('utf-8')
    config = TransportFixture(kubectl, verifier_binary, False).config
    namespace, uid = 'ai-sdlc-verifiers', None

    def command(args, value=None, timeout=45):
        result = subprocess.run(kubectl + args, input=wire(value) if isinstance(value, dict) else value,
                                capture_output=True, timeout=timeout)
        if result.returncode:
            raise ValueError('Verifier fixture Kubernetes operation failed')
        return result.stdout
    try:
        created = json.loads(command(['create', '-f', '-', '-o', 'json'], {
            'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
            'labels': {'pod-security.kubernetes.io/enforce': 'restricted'}}}))
        uid = created['metadata']['uid']
        command(['create', '-f', '-'], {'apiVersion': 'v1', 'kind': 'List', 'items': [
            {'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy', 'metadata': {'name': 'deny-all', 'namespace': namespace},
             'spec': {'podSelector': {}, 'policyTypes': ['Ingress', 'Egress']}},
            {'apiVersion': 'v1', 'kind': 'ServiceAccount', 'metadata': {'name': 'verifier', 'namespace': namespace}, 'automountServiceAccountToken': False},
            {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'Role', 'metadata': {'name': 'verifier', 'namespace': namespace}, 'rules': [
                {'apiGroups': [''], 'resources': ['pods', 'configmaps'], 'verbs': ['get', 'create', 'delete']},
                {'apiGroups': [''], 'resources': ['pods/log'], 'verbs': ['get']},
                {'apiGroups': ['networking.k8s.io'], 'resources': ['networkpolicies'], 'verbs': ['list']}]},
            {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'RoleBinding', 'metadata': {'name': 'verifier', 'namespace': namespace},
             'roleRef': {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Role', 'name': 'verifier'},
             'subjects': [{'kind': 'ServiceAccount', 'name': 'verifier', 'namespace': namespace}]}]})
        token = command(['create', 'token', 'verifier', '-n', namespace, '--duration=10m']).decode().strip()
        restricted = dict(APIServer=config['APIServer'], CA=config['CA'], BearerToken=token)
        result = candidate_verify(kubectl, worker, mailbox, candidate_binary, source_sha, 'verification',
                                  dict(Changes=changes, VerificationConfig=restricted, VerificationDriver=str(verifier_binary.resolve()), VerificationScenario=scenario))
        for kind in ('pods', 'configmaps'):
            resources = json.loads(command(['-n', namespace, 'get', kind, '-o', 'json']))['items']
            remaining = [item for item in resources if item['metadata']['name'] != 'kube-root-ca.crt']
            expected = 1 if scenario == 'lost-config-reply' and kind == 'configmaps' else 0
            if len(remaining) != expected:
                raise ValueError('Verifier resources differ from the expected recovery outcome')
    finally:
        if uid:
            cleanup_namespace(lambda *args, **kwargs: command(*args, **dict(kwargs, timeout=65)), namespace, uid)
    return dict(result, verifierNamespaceRemoved=True, verifierResourcesConfirmedRemoved=scenario != "lost-config-reply", separateServiceAccounts=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('kubectl', 'kubeconfig', 'worker-image', 'mailbox-image', 'source-sha'):
        parser.add_argument('--' + name, required=True)
    for name in ('candidate-binary', 'verifier-binary', 'candidate-directory', 'report'):
        parser.add_argument('--' + name, required=True, type=Path)
    parser.add_argument("--scenario", choices=("success", "revoke-config", "revoke-results", "lost-config-reply", "expire-config", "revoke-intent", "transient-policy"), default="success")
    args = parser.parse_args()
    result = verify([args.kubectl, '--kubeconfig', args.kubeconfig], args.worker_image, args.mailbox_image,
                    args.candidate_binary, args.verifier_binary, args.source_sha, args.candidate_directory, args.scenario)
    args.report.write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result, indent=2))


if __name__ == '__main__':
    main()
