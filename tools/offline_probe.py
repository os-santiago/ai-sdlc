"""Real offline-runtime checks with native positive controls, never live work."""
import json
from pathlib import Path
import secrets
import subprocess
import time

import network_probe

SCRIPT = Path(__file__).with_suffix('.js').read_text() + '\nmodule.exports.run();\n'
NETWORK_KEYS = {'tcp4', 'tcp6Mapped', 'udp4', 'udp6Mapped'}
LOCAL_KEYS = {'gvisor', 'loopback4', 'loopback6', 'nonRoot', 'capabilitiesDropped',
              'credentialMountsAbsent', 'rootReadOnly', 'workspaceWritable', 'externalInterfacesAbsent'}


def pod(namespace, name, image, runtime_class=None):
    value = network_probe.pod(namespace, name, image)
    spec = value['spec']
    spec.update(enableServiceLinks=False, activeDeadlineSeconds=300,
                volumes=[{'name': 'workspace', 'emptyDir': {'sizeLimit': '64Mi'}}])
    spec['securityContext']['fsGroup'] = 1000
    container = spec['containers'][0]
    container['resources']['limits'] = {'memory': '768Mi', 'cpu': '1', 'ephemeral-storage': '128Mi'}
    container['volumeMounts'] = [{'name': 'workspace', 'mountPath': '/workspaces'}]
    if name == 'server-a':
        container['command'] = ['node', '-e', SCRIPT, 'serve']
    if runtime_class:
        spec['runtimeClassName'] = runtime_class
    return value


def checked_network(raw, expected):
    result = json.loads(raw)
    if not isinstance(result, dict) or set(result) != NETWORK_KEYS or any(value != expected for value in result.values()):
        raise ValueError('Offline network probe did not establish the expected outcome')
    return result


def checked_local(raw):
    result = json.loads(raw)
    if not isinstance(result, dict) or set(result) != LOCAL_KEYS or any(value is not True for value in result.values()):
        raise ValueError('Offline sandbox local checks failed')
    return result


def verify(kubectl, image, runtime_class):
    namespace = 'ai-sdlc-offlinecheck-' + secrets.token_hex(6)
    uid = None
    observed_uid = None
    missing_class = 'ai-sdlc-missing-' + secrets.token_hex(6)
    missing_uid = None

    def call(args, value=None):
        result = subprocess.run([str(v) for v in kubectl] + args, text=True,
                                input=json.dumps(value) if value is not None else None,
                                capture_output=True, timeout=110)
        if result.returncode:
            raise ValueError('Offline runtime probe command failed; no denial inferred')
        return result.stdout

    def probe(name, mode, *args):
        return call(['-n', namespace, 'exec', name, '--', 'node', '-e', SCRIPT, mode, *args])

    try:
        created = json.loads(call(['create', '-f', '-', '-o', 'json'], {
            'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
            'labels': {'app.kubernetes.io/part-of': 'ai-sdlc-offline-conformance',
                       'pod-security.kubernetes.io/enforce': 'restricted'}}}))
        uid = created['metadata']['uid']
        # Deliberately no NetworkPolicy: native connectivity is the positive
        # control, while the offline runtime must enforce its own boundary.
        for name in ('server-a', 'client', 'offline'):
            call(['create', '-f', '-'], pod(namespace, name, image, runtime_class if name == 'offline' else None))
        call(['-n', namespace, 'wait', '--for=condition=Ready', 'pods', '--all', '--timeout=90s'])
        target = call(['-n', namespace, 'get', 'pod', 'server-a', '-o', 'jsonpath={.status.podIP}']).strip()
        checked_network(probe('client', 'network', target), 'reachable')
        local = checked_local(probe('offline', 'local'))
        checked_network(probe('offline', 'network', target), 'unreachable')
        # Positive recheck prevents a dead server from being counted as a deny.
        checked_network(probe('client', 'network', target), 'reachable')
        scc = call(['-n', namespace, 'exec', 'offline', '--', 'scc', '--version']).strip()
        if scc != '0.4.2':
            raise ValueError('Unexpected SCC version inside offline runtime')
        observed_uid = call(['-n', namespace, 'get', 'pod', 'offline', '-o', 'jsonpath={.metadata.uid}']).strip()
        if not observed_uid:
            raise ValueError('Offline probe Pod identity missing')
        missing = json.loads(call(['create', '-f', '-', '-o', 'json'], {
            'apiVersion': 'node.k8s.io/v1', 'kind': 'RuntimeClass',
            'metadata': {'name': missing_class}, 'handler': missing_class}))
        missing_uid = missing['metadata']['uid']
        call(['create', '-f', '-'], pod(namespace, 'missing-handler', image, missing_class))
        deadline = time.monotonic() + 30
        while True:
            missing_pod = json.loads(call(['-n', namespace, 'get', 'pod', 'missing-handler', '-o', 'json']))
            status = missing_pod.get('status', {})
            if status.get('phase') == 'Running' or any(
                    item.get('containerID') or item.get('state', {}).get('running')
                    or item.get('state', {}).get('terminated') for item in status.get('containerStatuses', [])):
                raise ValueError('Missing runtime unexpectedly started a container')
            events = json.loads(call(['-n', namespace, 'get', 'events',
                '--field-selector', 'involvedObject.uid=' + missing_pod['metadata']['uid'], '-o', 'json']))
            if any('no runtime for' in event.get('message', '') and missing_class in event.get('message', '')
                   for event in events['items']):
                break
            if time.monotonic() >= deadline:
                raise ValueError('Missing-handler fail-closed behavior was not established')
            time.sleep(1)
    finally:
        try:
            if uid:
                network_probe.cleanup_namespace(call, namespace, uid)
        finally:
            if missing_uid:
                call(['delete', '--raw', '/apis/node.k8s.io/v1/runtimeclasses/' + missing_class, '-f', '-'],
                     {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': missing_uid}})
    return dict(local, nativePositiveControls=True, tcp4Blocked=True, udp4Blocked=True,
                tcp6MappedBlocked=True, udp6MappedBlocked=True, sccVersion=scc,
                observedPodUID=observed_uid, temporaryResourcesRemoved=True,
                noNetworkPolicyRequired=True, missingHandlerFailsClosed=True)


def verify_nodes(kubectl, image, runtime_class, nodes):
    """Run the sandbox's local checks once on each prepared node.

    verify() proves the network boundary with native positive controls once;
    this proves that every node can start a sandboxed pod: one offline pod
    pinned to each node by its hostname label, the same local checks, and the
    node the scheduler actually used."""
    namespace = 'ai-sdlc-offlinenodes-' + secrets.token_hex(6)
    uid = None

    def call(args, value=None):
        result = subprocess.run([str(v) for v in kubectl] + args, text=True,
                                input=json.dumps(value) if value is not None else None,
                                capture_output=True, timeout=150)
        if result.returncode:
            raise ValueError('Offline runtime node probe command failed')
        return result.stdout

    results = {}
    try:
        created = json.loads(call(['create', '-f', '-', '-o', 'json'], {
            'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
            'labels': {'app.kubernetes.io/part-of': 'ai-sdlc-offline-conformance',
                       'pod-security.kubernetes.io/enforce': 'restricted'}}}))
        uid = created['metadata']['uid']
        names = {}
        for index, node in enumerate(nodes):
            name = 'offline-' + str(index)
            value = pod(namespace, name, image, runtime_class)
            value['spec']['nodeSelector'] = {'kubernetes.io/hostname': node}
            call(['create', '-f', '-'], value)
            names[node] = name
        call(['-n', namespace, 'wait', '--for=condition=Ready', 'pods', '--all', '--timeout=120s'])
        for node, name in names.items():
            scheduled = call(['-n', namespace, 'get', 'pod', name, '-o', 'jsonpath={.spec.nodeName}']).strip()
            if scheduled != node:
                raise ValueError('Offline node probe ran on an unexpected node')
            checked_local(call(['-n', namespace, 'exec', name, '--', 'node', '-e', SCRIPT, 'local']))
            results[node] = {'scheduledOn': scheduled, 'gvisor': True, 'localChecks': True}
    finally:
        if uid:
            network_probe.cleanup_namespace(call, namespace, uid)
    return results
