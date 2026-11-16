#!/usr/bin/env python3
"""Validate closed synthetic build manifests against the real Tekton API only."""
import argparse
import hashlib
import json
from pathlib import Path
import secrets
import subprocess

import konflux_fixture_guard as guard
import fixture_cleanup


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--cluster', default='ai-sdlc-konflux-reviewed')
    p.add_argument('--compiled', type=Path, required=True)
    p.add_argument('--output', type=Path, required=True)
    args = p.parse_args()
    if args.output.exists():
        raise ValueError('Use a new report path')
    state, tools = guard.installation_paths(args.cluster)
    owner = json.loads((state / 'konflux-ownership.json').read_text())
    if owner['cluster'] != args.cluster or owner['scope'] != 'isolated-konflux':
        raise ValueError('An owned isolated supply-chain fixture is required')
    kubectl = [str(tools / 'kubectl-1.35.8'), '--kubeconfig', str(state / 'kubeconfig')]
    namespace = 'ai-sdlc-builds'
    nonce = secrets.token_hex(16)
    uid = None
    create_attempted = False
    report = {'version': 1, 'status': 'failed', 'scope': 'server-side dry-run only',
              'autonomousE2E': False, 'workloadExecuted': False,
              'dependencies': 'synthetic compiler fixture identities; not operational custody',
              'accepted': [], 'cleanupComplete': False}

    def call(argv, value=None, check=True):
        return subprocess.run(kubectl + argv, input=json.dumps(value) if value is not None else None,
                              capture_output=True, text=True, timeout=30, check=check)

    def persist_report():
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(report, indent=2) + '\n')

    try:
        actual = json.loads(call(['get', 'namespace', 'kube-system', '-o', 'json']).stdout)
        if actual['metadata']['uid'] != owner['clusterUID']:
            raise ValueError('Cluster ownership changed')
        existing = json.loads(call(['get', 'namespaces', '-o', 'json']).stdout)['items']
        if any(x['metadata']['name'] == namespace for x in existing):
            raise ValueError('Refusing to adopt an existing build namespace')
        report['namespaceCreateIntent'] = {'name': namespace, 'nonce': nonce}
        persist_report()
        create_attempted = True
        created = json.loads(call(['create', '-f', '-', '-o', 'json'], {
            'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
            'labels': {'ai-sdlc.compiler-fixture': nonce, 'pod-security.kubernetes.io/enforce': 'restricted'}}}).stdout)
        uid = created['metadata']['uid']
        for phase in ('build', 'publish'):
            file = args.compiled / (phase + '.json')
            if file.is_symlink() or not file.is_file() or not 0 < file.stat().st_size <= 32768:
                raise ValueError('Bounded compiled fixture required')
            raw = file.read_bytes()
            value = json.loads(raw)
            if value.get('kind') != 'PipelineRun' or value['metadata'].get('namespace') != namespace:
                raise ValueError('Unexpected fixture kind or namespace')
            result = call(['create', '--dry-run=server', '-f', '-', '-o', 'json'], value)
            accepted = json.loads(result.stdout)
            if accepted['metadata']['name'] != value['metadata']['name']:
                raise ValueError('Admission changed workload identity')
            report['accepted'].append({'phase': phase, 'requestSHA256': hashlib.sha256(raw).hexdigest()})
        for resource in ('pipelineruns', 'taskruns', 'pods'):
            if json.loads(call(['get', resource, '-n', namespace, '-o', 'json']).stdout)['items']:
                raise ValueError('A dry-run fixture unexpectedly created execution resources')
        report['status'] = 'passed'
    except subprocess.CalledProcessError as exc:
        # These requests contain only public synthetic build data, no credentials.
        report['apiError'] = (exc.stderr or '')[-2000:]
        raise
    finally:
        def cleanup(checkpoint):
            nonlocal uid
            if uid is None and create_attempted:
                uid = fixture_cleanup.recover_namespace_uid(
                    lambda argv: call(argv).stdout, namespace, 'ai-sdlc.compiler-fixture', nonce)
                report['recoveredNamespaceUIDForCleanup'] = uid
            if uid:
                actual = json.loads(call(['get', 'namespace', namespace, '-o', 'json']).stdout)
                if actual['metadata']['uid'] != uid or actual['metadata']['labels'].get('ai-sdlc.compiler-fixture') != nonce:
                    raise ValueError('Refusing cleanup of replaced namespace')
                call(['delete', '--raw', '/api/v1/namespaces/' + namespace, '-f', '-'], {
                    'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': uid}})
                call(['wait', '--for=delete', 'namespace/' + namespace, '--timeout=25s'])
                report['cleanupComplete'] = True
        fixture_cleanup.finish_fixture(report, persist_report, cleanup)
    print(json.dumps(report))


if __name__ == '__main__':
    main()
