#!/usr/bin/env python3
"""Check bound local-PV API identity against an owned cluster, without any Pod or mount."""
import argparse
import json
import os
from pathlib import Path
import secrets
import re
import subprocess
import tempfile
import time
import konflux_fixture_guard as guard
import fixture_cleanup


def test_result(stdout):
    if not re.search(r'^--- PASS: TestRealBuildSlotAPICustody \(',stdout,re.MULTILINE) or re.search(r'^--- (SKIP|FAIL): TestRealBuildSlotAPICustody',stdout,re.MULTILINE):
        raise ValueError('Required real API test did not pass')
    matches=re.findall(r'AI_SDLC_SLOT_RESULT (\{[^\n]*\})',stdout)
    if len(matches)!=1:
        raise ValueError('Missing or ambiguous executed API controls')
    result=json.loads(matches[0])
    if set(result)!={'accepted','rejected'} or any(type(v) is not int or v<1 for v in result.values()):
        raise ValueError('Invalid executed API control counts')
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cluster', default='ai-sdlc-konflux-reviewed')
    parser.add_argument('--test-binary', required=True, type=Path)
    parser.add_argument('--output', required=True, type=Path)
    parser.add_argument('--inject-lost-ack', choices=('Namespace', 'PersistentVolume', 'PersistentVolumeClaim'))
    args = parser.parse_args()
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    state, tools = guard.installation_paths(args.cluster)
    owner = json.loads((state / 'konflux-ownership.json').read_text())
    if owner['cluster'] != args.cluster or owner['scope'] != 'isolated-konflux':
        raise ValueError('Owned isolated cluster required')
    command = [str(tools / 'kubectl-1.35.8'), '--kubeconfig', str(state / 'kubeconfig')]
    namespace = 'ai-sdlc-builds'
    nonce = secrets.token_hex(16)
    pvname = 'build-api-fixture-' + nonce
    identity = secrets.token_hex(32)
    intents = []
    report = {'status': 'failed', 'scope': 'isolated bound-volume API custody', 'autonomousE2E': False,
              'workloadExecuted': False, 'physicalMountValidated': False, 'cleanupComplete': False, 'createIntents': intents}

    def persist():
        (args.output / 'report.json').write_text(json.dumps(report, indent=2) + '\n')

    def call(arguments, value=None):
        result = subprocess.run(command + arguments, input=json.dumps(value) if value is not None else None,
                                capture_output=True, text=True, timeout=30)
        if result.returncode:
            raise ValueError('Fixture Kubernetes operation failed: ' + arguments[0])
        return result.stdout

    def get(kind, name=None, ns=None):
        return json.loads(call(['get', kind] + ([name] if name else []) + (['-n', ns] if ns else []) + ['-o', 'json']))

    def create(obj):
        def post(value):
            result = json.loads(call(['create', '-f', '-', '-o', 'json'], value))
            if args.inject_lost_ack == value['kind']:
                report['injectedLostAcknowledgement'] = value['kind']
                raise TimeoutError('Injected successful create acknowledgement loss')
            return result
        return fixture_cleanup.create_slot_resource(post, persist, intents, obj, owner['clusterUID'], nonce)

    try:
        if get('namespace', 'kube-system')['metadata']['uid'] != owner['clusterUID']:
            raise ValueError('Cluster identity changed')
        if any(n['metadata']['name'] == namespace for n in get('namespaces')['items']):
            raise ValueError('Refusing existing build namespace')
        nodes = get('nodes')['items']
        if len(nodes) != 1:
            raise ValueError('Single owned fixture node required')
        node = nodes[0]
        nodename = node['metadata']['name']
        ns = create({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
                    'labels': {'ai-sdlc.slot-api-fixture': nonce, 'pod-security.kubernetes.io/enforce': 'restricted'}}})
        pv = create({'apiVersion': 'v1', 'kind': 'PersistentVolume', 'metadata': {'name': pvname}, 'spec': {
            'capacity': {'storage': '4Gi'}, 'accessModes': ['ReadWriteOnce'], 'storageClassName': '', 'volumeMode': 'Filesystem',
            'persistentVolumeReclaimPolicy': 'Retain', 'local': {'path': '/var/local/ai-sdlc/build-slots/' + identity},
            'nodeAffinity': {'required': {'nodeSelectorTerms': [{'matchExpressions': [{'key': 'kubernetes.io/hostname', 'operator': 'In', 'values': [nodename]}]}]}}}})
        pvc = create({'apiVersion': 'v1', 'kind': 'PersistentVolumeClaim', 'metadata': {'name': 'build-slot-0', 'namespace': namespace},
                      'spec': {'accessModes': ['ReadWriteOnce'], 'storageClassName': '', 'volumeMode': 'Filesystem',
                               'resources': {'requests': {'storage': '4Gi'}}, 'volumeName': pvname}})
        deadline = time.monotonic() + 40
        while time.monotonic() < deadline:
            current = get('pvc', 'build-slot-0', namespace)
            if current['metadata']['uid'] != pvc['metadata']['uid']:
                raise ValueError('Fixture claim replaced')
            if current.get('status', {}).get('phase') == 'Bound':
                break
            time.sleep(1)
        else:
            raise ValueError('Fixture PVC never bound')
        if get('pods', ns=namespace)['items']:
            raise ValueError('API-only fixture unexpectedly has Pods')
        slot = {'clusterUID': owner['clusterUID'], 'namespaceUID': ns['metadata']['uid'], 'pvcUID': pvc['metadata']['uid'],
                'pvUID': pv['metadata']['uid'], 'nodeUID': node['metadata']['uid'], 'nodeBootID': node['status']['nodeInfo']['bootID'],
                'storageIdentitySHA256': identity, 'namespace': namespace, 'pvc': 'build-slot-0', 'node': nodename,
                'capacityBytes': 4 << 30, 'inodeLimit': 32768}
        for kind, name, scope, filename in [('pv', pvname, None, 'pv'), ('pvc', 'build-slot-0', namespace, 'pvc'),
                                             ('node', nodename, None, 'node'), ('lease', nodename, 'kube-node-lease', 'lease')]:
            (args.output / (filename + '.json')).write_text(json.dumps(get(kind, name, scope), indent=2) + '\n')
        cfg = json.loads(call(['config', 'view', '--raw', '--minify', '-o', 'json']))
        cluster, user = cfg['clusters'][0]['cluster'], cfg['users'][0]['user']
        config = {'APIServer': cluster['server'], 'CA': cluster['certificate-authority-data'],
                  'ClientCertificate': user['client-certificate-data'], 'ClientKey': user['client-key-data']}
        with tempfile.TemporaryDirectory(prefix='ai-sdlc-slot-api-') as directory:
            path = Path(directory) / 'private.json'
            with os.fdopen(os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600), 'w') as output:
                json.dump({'Config': config, 'Slot': slot}, output)
            result = subprocess.run([str(args.test_binary.resolve()), '-test.run', '^TestRealBuildSlotAPICustody$', '-test.v'],
                                    env=dict(os.environ, AI_SDLC_BUILD_SLOT_FIXTURE=str(path)), capture_output=True, text=True, timeout=100)
            (args.output / 'test.log').write_text(result.stdout)
            if result.returncode:
                raise ValueError('Real slot API test failed; see sanitized test.log')
        report.update(status='passed', **test_result(result.stdout))
    finally:
        def cleanup(checkpoint):
            if get('namespace', 'kube-system')['metadata']['uid'] != owner['clusterUID']:
                raise ValueError('Original cluster changed before cleanup')
            fixture_cleanup.recover_slot_resources(call, intents)
            checkpoint()
            for intent in reversed(intents):
                if intent['absent']:
                    continue
                prefix = '/api/v1/'
                if intent['namespace']:
                    prefix += 'namespaces/' + intent['namespace'] + '/'
                plural = {'namespace': 'namespaces', 'pv': 'persistentvolumes', 'pvc': 'persistentvolumeclaims'}[intent['resource']]
                call(['delete', '--raw', prefix + plural + '/' + intent['name'], '-f', '-'],
                     {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': intent['uid']}})
            deadline = time.monotonic() + 60
            while time.monotonic() < deadline:
                fixture_cleanup.recover_slot_resources(call, intents)
                if all(intent['absent'] for intent in intents):
                    report['cleanupComplete'] = True
                    break
                time.sleep(1)
            if not report['cleanupComplete']:
                raise ValueError('Original fixture cleanup not complete')
        fixture_cleanup.finish_fixture(report, persist, cleanup)
    print(json.dumps(report))


if __name__ == '__main__':
    main()
