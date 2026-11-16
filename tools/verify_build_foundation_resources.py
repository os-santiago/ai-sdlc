#!/usr/bin/env python3
"""Round-trip real foundation resources in a disposable namespace, without Pods."""
import argparse
import copy
import json
from pathlib import Path
import re
import secrets
import subprocess
import time

from build_foundation_state import Resources, ANNOTATION, identity, UID
from fixture_cleanup import finish_fixture
from konflux_fixture_guard import installation_paths
from network_probe import cleanup_namespace

ROOT = Path(__file__).resolve().parents[1]


def cleanup_uid(actual, entry):
    meta = actual.get('metadata', {})
    uid = meta.get('uid', '')
    if (not UID.fullmatch(uid) or meta.get('annotations', {}).get(ANNOTATION) != entry['nonce']
            or entry.get('uid') not in (None, uid)):
        raise ValueError('Original administrative fixture cleanup identity differs')
    return uid


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cluster', required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args(argv)
    if not re.fullmatch(r'[a-z][a-z0-9-]{0,35}', args.cluster) or args.output.exists() or args.output.is_symlink():
        raise ValueError('Owned cluster and new evidence directory required')
    state_dir, tools = installation_paths(args.cluster)
    marker = state_dir/'konflux-ownership.json'
    if not marker.exists():
        marker = state_dir/'ownership.json'
    owner = json.loads(marker.read_text())
    if owner.get('cluster') != args.cluster or not UID.fullmatch(owner.get('clusterUID', '')):
        raise ValueError('Existing installation ownership required')
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    kubectl = [str(tools/'kubectl-1.35.8'), '--kubeconfig', str(state_dir/'kubeconfig'), '--context', 'kind-'+args.cluster]
    nonce = secrets.token_hex(12)
    namespace = 'foundation-resources-'+nonce
    pv_name = namespace+'-pv'
    desired = []
    state = {'resources': {}}
    report = {'status': 'failed', 'namespace': namespace, 'clusterUID': owner['clusterUID'],
              'scope': 'isolated administrative resource journal; no workload, mount or delivery',
              'podsCreated': False, 'storageMounted': False, 'autonomousE2E': False, 'cleanupComplete': False}
    deadline = time.monotonic()+180
    def save():
        (args.output/'report.json').write_text(json.dumps(report, indent=2)+'\n')
    def call(arguments, value=None):
        remaining = deadline-time.monotonic()
        if remaining <= 0:
            raise TimeoutError('Administrative fixture budget exhausted')
        return subprocess.run(kubectl+arguments, input=json.dumps(value) if value is not None else None,
                              capture_output=True, text=True, check=True, timeout=min(75, remaining)).stdout
    def fence():
        actual = json.loads(call(['get', 'namespace', 'kube-system', '-o', 'json']))
        if actual['metadata']['uid'] != owner['clusterUID']:
            raise ValueError('Original cluster was replaced')
    def get(obj):
        meta = obj['metadata']
        raw = call(['get', obj['kind'], meta['name'], *(['-n', meta['namespace']] if meta.get('namespace') else []),
                    '--ignore-not-found', '-o', 'json'])
        return json.loads(raw) if raw.strip() else None
    def create(obj):
        fence()
        return json.loads(call(['create', '-f', '-', '-o', 'json'], obj))
    resources = Resources(args.output/'state.json', state, get, create)
    def ensure(obj):
        desired.append(copy.deepcopy(obj))
        return resources.ensure(obj)
    try:
        fence()
        ensure({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
                'labels': {'pod-security.kubernetes.io/enforce': 'restricted'}}})
        blueprint = json.loads((ROOT/'runtime/k8s/deploy/konflux/build-foundation.json').read_text())
        for obj in blueprint['items']:
            obj['metadata']['namespace'] = namespace
            ensure(obj)
        pvc = ensure({'apiVersion': 'v1', 'kind': 'PersistentVolumeClaim', 'metadata': {'name': 'slot', 'namespace': namespace},
                      'spec': {'storageClassName': '', 'volumeName': pv_name, 'accessModes': ['ReadWriteOnce'],
                               'resources': {'requests': {'storage': '4Gi'}}}})
        ensure({'apiVersion': 'v1', 'kind': 'PersistentVolume', 'metadata': {'name': pv_name}, 'spec': {
            'capacity': {'storage': '4Gi'}, 'accessModes': ['ReadWriteOnce'], 'persistentVolumeReclaimPolicy': 'Retain',
            'storageClassName': '', 'local': {'path': '/tmp/'+namespace},
            'claimRef': {'apiVersion': 'v1', 'kind': 'PersistentVolumeClaim', 'namespace': namespace,
                         'name': 'slot', 'uid': pvc['metadata']['uid']},
            'nodeAffinity': {'required': {'nodeSelectorTerms': [{'matchExpressions': [
                {'key': 'kubernetes.io/hostname', 'operator': 'In', 'values': [args.cluster+'-control-plane']}]}]}}}})
        call(['wait', '-n', namespace, '--for=jsonpath={.status.phase}=Bound', 'pvc/slot', '--timeout=30s'])
        for obj in desired:
            resources.ensure(obj)
        if json.loads(call(['get', 'pods', '-n', namespace, '-o', 'json']))['items']:
            raise ValueError('Unexpected workload in administrative fixture')
        report.update(status='passed', resourcesReapplied=len(desired), originalUIDsRetained=True)
    except BaseException as error:
        report['status'] = 'failed'
        report['failureType'] = type(error).__name__
        raise
    finally:
        # Cleanup receives its own budget and recovers only the original nonce.
        deadline = time.monotonic()+120
        def cleanup(checkpoint):
            fence()
            for kind in ('Namespace', 'PersistentVolume'):
                for obj in desired:
                    if obj['kind'] != kind:
                        continue
                    entry = state['resources'].get(identity(obj))
                    if entry is None:
                        continue  # No create intent, therefore no deletion authority.
                    actual = get(obj)
                    if actual is None:
                        continue
                    if identity(actual) != identity(obj):
                        raise ValueError('Fixture cleanup object identity differs')
                    uid = cleanup_uid(actual, entry)
                    fence()
                    if kind == 'Namespace':
                        cleanup_namespace(call, namespace, uid)
                    else:
                        call(['delete', '--raw', '/api/v1/persistentvolumes/'+pv_name, '-f', '-'],
                             {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': uid}})
                        call(['wait', '--for=delete', 'pv/'+pv_name, '--timeout=30s'])
            report['cleanupComplete'] = True
        finish_fixture(report, save, cleanup)
    print(json.dumps(report))
    return 0


if __name__ == '__main__':
    main()
