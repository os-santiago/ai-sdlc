#!/usr/bin/env python3
"""Run the closed transport against an owned Konflux fixture, never live workflow state."""
import argparse
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import tempfile
import time

import konflux_fixture_guard as guard
import fixture_cleanup
import saferus_build_fixture
import supply_chain_install


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--preflight-only', action='store_true')
    parser.add_argument('--cluster', default='ai-sdlc-konflux-reviewed')
    parser.add_argument('--test-binary', required=True, type=Path)
    parser.add_argument('--publisher-image', required=True)
    parser.add_argument('--publisher-archive', required=True, type=Path)
    parser.add_argument('--source-sha', required=True)
    parser.add_argument('--tree-sha', required=True)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    if not re.fullmatch(r'localhost/ai-sdlc-artifact-builder@sha256:[a-f0-9]{64}', args.publisher_image):
        raise ValueError('Exact local publisher required')
    if any(not re.fullmatch(r'[a-f0-9]{40}', value) for value in (args.source_sha, args.tree_sha)):
        raise ValueError('Exact source and tree required')
    if not args.test_binary.is_file() or args.test_binary.stat().st_size > 128 << 20:
        raise ValueError('Bounded compiled fixture binary required')
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    state, tools = guard.installation_paths(args.cluster)
    owner = json.loads((state / 'konflux-ownership.json').read_text())
    if owner['cluster'] != args.cluster or owner['scope'] != 'isolated-konflux':
        raise ValueError('Owned isolated Konflux installation required')
    provider = owner['provider']
    node = args.cluster + '-control-plane'
    kubectl = [str(tools / 'kubectl-1.35.8'), '--kubeconfig', str(state / 'kubeconfig')]
    deadline = supply_chain_install.Deadline(1400)
    nonce = secrets.token_hex(12)
    identity = secrets.token_hex(32)
    namespace = 'ai-sdlc-builds'
    pvname = 'closed-build-fixture-' + nonce
    root = '/tmp/ai-sdlc-closed-build-' + nonce
    slotpath = '/var/local/ai-sdlc/build-slots/' + identity
    mount_source = 'ai-sdlc-slot-' + nonce
    node_id = None
    namespace_uid = None
    namespace_create_attempted = False
    pv_uid = None
    mounted = False
    prepared = False
    owned_policies = []
    report = {'status': 'running', 'scope': 'isolated closed transport integration fixture',
              'autonomousE2E': False, 'syntheticAuthority': True, 'liveWorkflowAdvanced': False,
              'releaseAuthorized': False, 'workloadExecuted': False, 'preflightOnly': args.preflight_only, 'cleanupComplete': False, 'sourceSHA': args.source_sha,
              'treeSHA': args.tree_sha, 'publisherImage': args.publisher_image}

    def save():
        (args.output / 'report.json').write_text(json.dumps(report, indent=2) + '\n')

    def call(arguments, value=None, check=True, timeout=30):
        result = deadline.run(kubectl + arguments, input=json.dumps(value) if value is not None else None,
                              capture_output=True, check=False, limit=timeout)
        if check and result.returncode:
            raise ValueError('Fixture Kubernetes operation failed: ' + str(arguments[:2]))
        return result

    def get(kind, name=None, ns=namespace):
        return json.loads(call(['get', kind] + ([name] if name else []) + (['-n', ns] if ns else []) + ['-o', 'json']).stdout)

    def create(value):
        return json.loads(call(['create', '-f', '-', '-o', 'json'], value).stdout)

    def node_call(arguments, **kwargs):
        return guard.owned_node_exec(deadline.run, provider, node, node_id, arguments, **kwargs)

    def copy_to(source, destination):
        node_call(['test', '-d', root])
        deadline.run([provider, 'cp', str(source), node_id + ':' + destination], capture_output=True, limit=30)

    def execute(phase, payload):
        with tempfile.TemporaryDirectory(prefix='ai-sdlc-closed-private-') as directory:
            path = Path(directory) / 'input.json'
            with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'w') as output:
                json.dump(dict(payload, Phase=phase, OutputDirectory=root + '/evidence'), output)
            copy_to(path, root + '/private.json')
        # Runtime tests log fixed diagnostics only. Private credentials never enter argv.
        result = deadline.run([provider, 'exec', node_id, 'env', 'AI_SDLC_CLOSED_BUILD_FIXTURE=' + root + '/private.json',
                               root + '/test', '-test.run', '^TestRealClosedBuildTransport$', '-test.v'],
                              capture_output=True, check=False, limit=1000 if phase == 'execute' else 60)
        (args.output / (phase + '.log')).write_text(result.stdout)
        if result.returncode:
            raise ValueError('Closed transport fixture failed at ' + phase + '; see sanitized log')

    def node_json(filename):
        return json.loads(node_call(['cat', root + '/evidence/' + filename]).stdout)

    def collect():
        if not namespace_uid:
            return
        ns = get('namespace', namespace, None)
        if ns['metadata']['uid'] != namespace_uid:
            raise ValueError('Fixture namespace replaced before evidence collection')
        if pv_uid:
            for kind, name, scope, filename in [('pv', pvname, None, 'slot-pv'), ('pvc', 'build-slot-0', namespace, 'slot-pvc'),
                                               ('node', node, None, 'slot-node'), ('lease', node, 'kube-node-lease', 'slot-lease')]:
                (args.output / (filename + '.json')).write_text(json.dumps(get(kind, name, scope), indent=2) + '\n')
        runs = get('pipelineruns')['items']
        tasks = get('taskruns')['items']
        pods = get('pods')['items']
        report['runUIDs'] = [r['metadata']['uid'] for r in runs]
        if any(p.get('status', {}).get('containerStatuses') or p.get('status', {}).get('initContainerStatuses') for p in pods):
            report['workloadExecuted'] = True
        report['archivedTaskUIDs'] = []
        for obj in runs + tasks + pods:
            name = obj['metadata']['name']
            (args.output / (obj['kind'].lower() + '-' + name + '.json')).write_text(json.dumps(obj, indent=2) + '\n')
        archived = set()
        for pod in pods:
            complete = True
            for container in pod['spec'].get('containers', []):
                result = call(['logs', '-n', namespace, pod['metadata']['name'], '-c', container['name'], '--limit-bytes=2097152'], check=False)
                (args.output / (pod['metadata']['name'] + '-' + container['name'] + '.log')).write_text(result.stdout)
                complete = complete and result.returncode == 0
            if complete:
                archived.add(pod['metadata']['uid'])
        report['archivedTaskUIDs'] = [t['metadata']['uid'] for t in tasks if all(p['metadata']['uid'] in archived for p in pods if guard.has_owner(p, 'TaskRun', t['metadata']['uid']))]
        save()

    try:
        if get('namespace', 'kube-system', None)['metadata']['uid'] != owner['clusterUID']:
            raise ValueError('Cluster identity changed')
        if any(n['metadata']['name'] == namespace for n in get('namespaces', ns=None)['items']):
            raise ValueError('Refusing existing build namespace')
        info = json.loads(deadline.run([provider, 'inspect', node], capture_output=True, limit=15).stdout)[0]
        if info['Config']['Labels'].get('io.x-k8s.kind.cluster') != args.cluster:
            raise ValueError('Unexpected kind node')
        node_id = info['Id']
        node_obj = get('node', node, None)
        report['nodeID'] = node_id
        report['publisherArchiveSHA256'] = guard.validate_oci_archive(args.publisher_archive, args.publisher_image)
        with args.publisher_archive.open('rb') as archive:
            node_call(['ctr', '-n', 'k8s.io', 'images', 'import', '--local', '--digests', '--base-name', 'localhost/ai-sdlc-artifact-builder', '-'], stdin=archive)
        inspected = json.loads(node_call(['crictl', 'inspecti', args.publisher_image]).stdout)
        if inspected['status'].get('repoDigests') != [args.publisher_image]:
            raise ValueError('Publisher digest alias ambiguity')
        report['namespaceCreateIntent'] = {'name': namespace, 'nonce': nonce}
        save()
        namespace_create_attempted = True
        ns = create({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace, 'labels': {
            'ai-sdlc.closed-build-fixture': nonce, 'pod-security.kubernetes.io/enforce': 'restricted'}}})
        namespace_uid = ns['metadata']['uid']
        report['namespaceUID'] = namespace_uid
        save()
        node_call(['bash', '-ceu', 'mkdir -m 700 "$1"; printf "%s" "$2" > "$1/owner"; mkdir -m 700 "$1/evidence"; mkdir -p /var/local/ai-sdlc/build-slots; mkdir -m 700 "$3"', 'fixture', root, namespace_uid, slotpath])
        prepared = True
        copy_to(args.test_binary, root + '/test')
        node_call(['mount', '-t', 'tmpfs', '-o', 'size=4294967296,nr_inodes=32768,mode=0755,uid=0,gid=0', mount_source, slotpath])
        mounted = True
        mounted_info = json.loads(node_call(['findmnt', '-J', '-M', slotpath, '-o', 'SOURCE,TARGET,FSTYPE,OPTIONS']).stdout)['filesystems'][0]
        if mounted_info['source'] != mount_source or mounted_info['fstype'] != 'tmpfs':
            raise ValueError('Dedicated tmpfs mismatch')
        report['mount'] = mounted_info
        pvc = create({'apiVersion': 'v1', 'kind': 'PersistentVolumeClaim', 'metadata': {'name': 'build-slot-0', 'namespace': namespace},
                      'spec': {'storageClassName': '', 'volumeName': pvname, 'accessModes': ['ReadWriteOnce'], 'resources': {'requests': {'storage': '4Gi'}}}})
        pv = create({'apiVersion': 'v1', 'kind': 'PersistentVolume', 'metadata': {'name': pvname}, 'spec': {
            'capacity': {'storage': '4Gi'}, 'accessModes': ['ReadWriteOnce'], 'persistentVolumeReclaimPolicy': 'Retain', 'storageClassName': '',
            'local': {'path': slotpath}, 'claimRef': {'apiVersion': 'v1', 'kind': 'PersistentVolumeClaim', 'namespace': namespace, 'name': 'build-slot-0', 'uid': pvc['metadata']['uid']},
            'nodeAffinity': {'required': {'nodeSelectorTerms': [{'matchExpressions': [{'key': 'kubernetes.io/hostname', 'operator': 'In', 'values': [node]}]}]}}}})
        pv_uid = pv['metadata']['uid']
        call(['wait', '-n', namespace, '--for=jsonpath={.status.phase}=Bound', 'pvc/build-slot-0', '--timeout=25s'])
        report.update(pvUID=pv_uid, pvcUID=pvc['metadata']['uid'])
        save()
        for policy in saferus_build_fixture.policies(namespace)[:2]:
            create(policy)
        create({'apiVersion': 'v1', 'kind': 'LimitRange', 'metadata': {'name': 'build-budget', 'namespace': namespace}, 'spec': {'limits': [{
            'type': 'Container', 'default': {'cpu': '100m', 'memory': '256Mi', 'ephemeral-storage': '64Mi'},
            'defaultRequest': {'cpu': '100m', 'memory': '64Mi', 'ephemeral-storage': '64Mi'}}]}})
        secret = get('secret', 'internal-registry-client-auth', 'kind-registry')
        create({'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'registry-auth', 'namespace': namespace},
                'type': 'kubernetes.io/dockerconfigjson', 'data': {'.dockerconfigjson': secret['data']['.dockerconfigjson']}})
        for phase in ('build', 'publish'):
            sa = {'apiVersion': 'v1', 'kind': 'ServiceAccount', 'metadata': {'name': phase, 'namespace': namespace}, 'automountServiceAccountToken': False}
            if phase == 'publish':
                sa['secrets'] = [{'name': 'registry-auth'}]
            create(sa)
        # Tekton mounts these optional bundle names even when no controller
        # creates them. Install explicit immutable empty bundles for this
        # isolated profile so absence cannot silently become new trust later.
        for name in ('config-trusted-cabundle', 'config-service-cabundle'):
            create({'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': name, 'namespace': namespace},
                    'immutable': True, 'data': {}})
        slot = {'clusterUID': owner['clusterUID'], 'namespaceUID': namespace_uid, 'pvcUID': pvc['metadata']['uid'], 'pvUID': pv_uid,
                'nodeUID': node_obj['metadata']['uid'], 'nodeBootID': node_obj['status']['nodeInfo']['bootID'], 'storageIdentitySHA256': identity,
                'namespace': namespace, 'pvc': 'build-slot-0', 'node': node, 'capacityBytes': 4 << 30, 'inodeLimit': 32768}
        cfg = json.loads(call(['config', 'view', '--raw', '--minify', '-o', 'json']).stdout)
        cluster, user = cfg['clusters'][0]['cluster'], cfg['users'][0]['user']
        payload = {'Config': {'APIServer': 'https://127.0.0.1:6443', 'CA': cluster['certificate-authority-data'],
                             'ClientCertificate': user['client-certificate-data'], 'ClientKey': user['client-key-data']},
                   'Slot': slot, 'PublisherImage': args.publisher_image, 'SourceSHA': args.source_sha, 'TreeSHA': args.tree_sha}
        execute('definitions', payload)
        for filename, resource in [('mutation.json', 'mutatingadmissionpolicies'), ('mutation-binding.json', 'mutatingadmissionpolicybindings'),
                                   ('policy.json', 'validatingadmissionpolicies'), ('binding.json', 'validatingadmissionpolicybindings')]:
            obj = create(node_json(filename))
            owned_policies.append((resource, obj['metadata']['name'], obj['metadata']['uid']))
            report['ownedPolicies'] = list(owned_policies)
            save()
        time.sleep(3)
        execute('trust', payload)
        create(node_json('build-trust.json'))
        if not args.preflight_only:
            report['workloadExecuted'] = None
            report['workloadMayHaveExecuted'] = True
            save()
        execute('preflight' if args.preflight_only else 'execute', payload)
        report['transport'] = node_json('transport-report.json')
        if report['transport'].get('status') != 'passed':
            raise ValueError('Transport did not complete both original phases')
        report['workloadExecuted'] = not args.preflight_only
        report['status'] = 'passed'
        save()
    except BaseException as exc:
        report['status'] = 'failed'
        report['failureType'] = type(exc).__name__
        save()
        raise
    finally:
        deadline = supply_chain_install.Deadline(180)
        def cleanup(checkpoint):
            nonlocal namespace_uid
            if namespace_uid is None and namespace_create_attempted:
                namespace_uid = fixture_cleanup.recover_namespace_uid(
                    lambda argv: call(argv).stdout, namespace, 'ai-sdlc.closed-build-fixture', nonce)
                report['recoveredNamespaceUIDForCleanup'] = namespace_uid
            guard.collect_best_effort(collect, report)
            def archive_reports():
                if prepared:
                    # Only sanitized, explicitly named evidence is copied out; never private.json.
                    for filename in ('transport-report.json', 'profile.json', 'build-trust.json', 'policy.json', 'binding.json', 'mutation.json', 'mutation-binding.json'):
                        check = deadline.run([provider, 'exec', node_id, 'test', '-f', root + '/evidence/' + filename], capture_output=True, check=False, limit=10)
                        if check.returncode == 0:
                            deadline.run([provider, 'cp', node_id + ':' + root + '/evidence/' + filename, str(args.output / filename)], capture_output=True, limit=15)
            guard.collect_best_effort(archive_reports, report)
            if namespace_uid:
                guard.cleanup_archived_namespace(lambda a, v=None: call(a, v).stdout, namespace, namespace_uid,
                                                 report.get('archivedTaskUIDs', []), report.get('runUIDs', []))
            for resource, name, uid in reversed(owned_policies):
                call(['delete', '--raw', '/apis/admissionregistration.k8s.io/v1/' + resource + '/' + name, '-f', '-'],
                     {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': uid}})
            if pv_uid:
                call(['delete', '--raw', '/api/v1/persistentvolumes/' + pvname, '-f', '-'],
                     {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': pv_uid}})
                call(['wait', '--for=delete', 'pv/' + pvname, '--timeout=30s'])
            if prepared:
                node_call(['bash', '-ceu', 'test "$(cat "$1/owner")" = "$2"; if mountpoint -q "$3"; then test "$(findmnt -n -M "$3" -o SOURCE)" = "$4"; umount "$3"; fi; rmdir "$3"; rm -rf -- "$1"', 'fixture', root, namespace_uid, slotpath, mount_source])
            report['cleanupComplete'] = True
        fixture_cleanup.finish_fixture(report, save, cleanup)
    print(json.dumps(report))


if __name__ == '__main__':
    main()
