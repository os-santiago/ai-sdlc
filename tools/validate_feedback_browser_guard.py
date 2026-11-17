#!/usr/bin/env python3
"""Exercise browser residual-work denial in an explicitly owned kind fixture."""
import argparse
import json
from pathlib import Path
import re
import secrets
import subprocess
import time

from bootstrap_prepare import Preparation
from install_upgrade import Source
from network_probe import cleanup_namespace, pod
from review_feedback_runtime_upgrade import no_browser_execution


def namespace_identity(value, owner, expected=None):
    uid = Preparation.identity(value)
    metadata = value.get('metadata', {})
    if (metadata.get('name') != 'ai-sdlc-browser'
            or metadata.get('labels', {}).get('ai-sdlc.io/feedback-guard-fixture') != owner
            or expected is not None and uid != expected):
        raise ValueError('Original owned fixture namespace changed')
    return uid


def verify(kubectl, cluster_uid, image, runner=None):
    runner = runner or subprocess.run
    started = time.monotonic()
    deadline, overall_deadline = started + 180, started + 250
    cleanup_remaining = 70
    namespace = 'ai-sdlc-browser'
    owner = secrets.token_hex(16)
    result = {'scope': 'actual Kubernetes browser residual-work guard only',
              'workflowEvidence': False, 'installedRuntimeValidated': False,
              'autonomousEndToEndValidated': False, 'scenarios': [], 'cleanup': [],
              'unresolvedNamespaceCreation': False}
    source = Source(Path(__file__).resolve().parents[1])
    result['sourceRevision'] = source.git('rev-parse', 'HEAD').decode().strip()
    result['dirty'] = bool(source.git('status', '--porcelain'))

    def call(args, value=None, *, cleanup=False):
        nonlocal cleanup_remaining
        began = time.monotonic()
        remaining = min(cleanup_remaining, overall_deadline-began) if cleanup else min(20, deadline-began)
        if remaining <= 0:
            raise ValueError('Original fixture observation deadline expired')
        try:
            reply = runner(kubectl + args, input=json.dumps(value) if value is not None else None,
                           capture_output=True, text=True, timeout=remaining)
        finally:
            if cleanup:
                # One shared, monotonically consumed cleanup budget, including
                # cluster guards, DELETE and waits across all three cases.
                cleanup_remaining = max(0, cleanup_remaining - (time.monotonic()-began))
        if reply.returncode or len(reply.stdout) > 4*1024*1024:
            raise ValueError('Bounded fixture Kubernetes operation failed')
        return reply.stdout

    def scope(*, cleanup=False):
        current = json.loads(call(['get', 'namespace', 'kube-system', '-o', 'json'], cleanup=cleanup))
        if Preparation.identity(current) != cluster_uid:
            raise ValueError('Original owned fixture cluster changed')

    class Observation:
        def get(self, kind, name=None, namespace=None):
            scope()
            return json.loads(call(['get', kind, *([name] if name else []),
                                   *(['-n', namespace] if namespace else []), '-o', 'json']))

    previous_uid = None
    try:
        for kind in ('Pod', 'Job', 'CronJob'):
            uid = None
            if cleanup_remaining <= 0:
                raise ValueError('Original fixture cleanup budget exhausted')
            scope()
            # CREATE, never apply/adopt: an existing operational namespace is
            # an error. Unknown creation acknowledgement is not cleanup custody.
            try:
                result['unresolvedNamespaceCreation'] = True
                created = json.loads(call(['create', '-f', '-', '-o', 'json'], {
                    'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
                    'labels': {'ai-sdlc.io/feedback-guard-fixture': owner,
                               'pod-security.kubernetes.io/enforce': 'restricted'}}}))
                uid = namespace_identity(created, owner)
                result['unresolvedNamespaceCreation'] = False
                def owned_namespace():
                    scope()
                    current = json.loads(call(['get', 'namespace', namespace, '-o', 'json']))
                    namespace_identity(current, owner, uid)
                owned_namespace()
                call(['create', '-f', '-'], {'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy',
                    'metadata': {'name': 'deny-all', 'namespace': namespace},
                    'spec': {'podSelector': {}, 'policyTypes': ['Ingress', 'Egress']}})
                observation = Observation()
                no_browser_execution(observation, uid)
                if previous_uid is not None:
                    try:
                        no_browser_execution(observation, previous_uid)
                    except ValueError as error:
                        if str(error) != 'Original browser namespace changed during feedback maintenance':
                            raise
                    else:
                        raise ValueError('Recreated fixture namespace adopted')
                template = pod(namespace, 'residual', image)
                template['spec']['schedulingGates'] = [{'name': 'ai-sdlc.io/observation-only'}]
                if kind == 'Pod':
                    resource = template
                else:
                    job = {'suspend': True, 'backoffLimit': 0,
                           'template': {'metadata': {'labels': {'fixture': owner}}, 'spec': template['spec']}}
                    resource = {'apiVersion': 'batch/v1', 'kind': kind,
                                'metadata': {'name': 'residual', 'namespace': namespace},
                                'spec': job if kind == 'Job' else
                                        {'suspend': True, 'schedule': '0 0 1 1 *', 'jobTemplate': {'spec': job}}}
                owned_namespace()
                residue = json.loads(call(['create', '-f', '-', '-o', 'json'], resource))
                Preparation.identity(residue)
                owned_namespace()
                if kind == 'Pod':
                    if (residue['spec'].get('schedulingGates') != template['spec']['schedulingGates']
                            or residue['spec'].get('nodeName')):
                        raise ValueError('Fixture Pod lost its scheduling gate')
                elif residue['spec'].get('suspend') is not True:
                    raise ValueError('Fixture controller lost its suspension')
                try:
                    no_browser_execution(observation, uid)
                except ValueError as error:
                    if str(error) != 'Browser execution or cleanup resources remain':
                        raise
                else:
                    raise ValueError('Residual browser work accepted')
                result['scenarios'].append({'kind': kind, 'emptyAccepted': True, 'residueRejected': True,
                                            'replacementRejected': previous_uid is not None})
                previous_uid = uid
            finally:
                if uid is not None:
                    scope(cleanup=True)
                    cleanup_namespace(lambda args, value=None: call(args, value, cleanup=True), namespace, uid)
                    result['cleanup'].append({'kind': kind, 'originalNamespaceRemoved': True})
        result['status'] = 'passed'
    except BaseException as error:
        result.update(status='failed', errorType=type(error).__name__)
        # Do not serialize subprocess argv, credentials or raw Kubernetes errors.
    result['cleanupSecondsUsed'] = round(70-cleanup_remaining, 3)
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('kubectl', 'kubeconfig', 'context', 'cluster-uid', 'image'):
        parser.add_argument('--' + name, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args(argv)
    if (not re.fullmatch(r'[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}', args.cluster_uid)
            or not re.fullmatch(r'[a-z0-9./_-]+@sha256:[a-f0-9]{64}', args.image)):
        parser.error('Explicit original cluster UID and immutable fixture image required')
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    result = verify([args.kubectl, '--kubeconfig', args.kubeconfig, '--context', args.context,
                     '--request-timeout=10s'], args.cluster_uid, args.image)
    (args.output / 'report.json').write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result))
    return 0 if result['status'] == 'passed' else 1


if __name__ == '__main__':
    raise SystemExit(main())
