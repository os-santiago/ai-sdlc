#!/usr/bin/env python3
"""Real Firefox/gVisor contract fixture with synthetic UI and API, not E2E proof."""
import argparse
import base64
import hashlib
import json
from pathlib import Path
import re
import secrets
import subprocess
import tempfile
import time

from network_probe import cleanup_namespace
import browser_image
from release_target_oci import bounded_output


def guarded_call(raw_call, namespace, uid, cluster_uid, args, value=None):
    def guard():
        cluster = json.loads(raw_call(['get', 'namespace', 'kube-system', '-o', 'json']))['metadata']
        current = json.loads(raw_call(['get', 'namespace', namespace, '-o', 'json']))['metadata']
        if (cluster['uid'] != cluster_uid or cluster.get('deletionTimestamp')
                or current['uid'] != uid or current.get('deletionTimestamp')
                or current.get('labels', {}).get('ai-sdlc.browser-fixture') != namespace):
            raise ValueError('Original browser fixture scope changed')
    if uid:
        guard()
    result = raw_call(args, value)
    if uid:
        guard()
    return result


def verify(kubectl, image, cluster_uid, provider='podman'):
    if provider not in ('podman', 'docker'):
        raise ValueError('Supported node provider required')
    namespace = 'ai-sdlc-browser-check-'+secrets.token_hex(6)
    uid = None
    if not re.fullmatch(r'(localhost/ai-sdlc-browser-verifier|quay.io/os-santiago/ai-sdlc)@sha256:[a-f0-9]{64}', image):
        raise ValueError('immutable browser image required')
    deadline = time.monotonic() + 360
    def raw_call(args, value=None):
        remaining = min(70, deadline-time.monotonic())
        if remaining <= 0:
            raise ValueError('fixture deadline')
        result = subprocess.run(kubectl+args, input=json.dumps(value) if value is not None else None,
                                text=True, capture_output=True, timeout=remaining)
        if result.returncode:
            raise ValueError('browser fixture Kubernetes operation failed')
        return result.stdout
    def call(args, value=None):
        return guarded_call(raw_call, namespace, uid, cluster_uid, args, value)
    def get(kind, name, namespace=None):
        return json.loads(call(['get', kind, name, *(['-n', namespace] if namespace else []), '-o', 'json']))
    def create(obj):
        return json.loads(call(['create', '-f', '-', '-o', 'json'], obj))
    if get('namespace', 'kube-system')['metadata']['uid'] != cluster_uid:
        raise ValueError('fixture cluster ownership differs')
    nodes = json.loads(call(['get', 'nodes', '-o', 'json']))['items']
    if len(nodes) != 1:
        raise ValueError('Single owned fixture node required')
    node = nodes[0]['metadata']['name']
    metadata = json.loads(subprocess.check_output([provider, 'inspect', node], timeout=10))[0]
    if metadata.get('Config', {}).get('Labels', {}).get('io.x-k8s.kind.cluster') != node.removesuffix('-control-plane'):
        raise ValueError('Fixture node ownership differs')
    node_id = metadata['Id']
    def read_image(digest, limit):
        current = json.loads(subprocess.check_output([provider, 'inspect', node], timeout=10))[0]
        if current['Id'] != node_id:
            raise ValueError('Fixture node replaced')
        return bounded_output([provider, 'exec', node_id, 'ctr', '-n', 'k8s.io', 'content', 'get', digest], limit)
    report = {'scope': 'synthetic-browser-component', 'autonomousE2E': False,
              'realSaferusAPI': False, 'image': image, 'scenarios': [], 'cleanupComplete': False}
    security = {'runAsNonRoot': True, 'runAsUser': 1001, 'runAsGroup': 1001,
                'fsGroup': 1001, 'seccompProfile': {'type': 'RuntimeDefault'}}
    container_security = {'readOnlyRootFilesystem': True, 'allowPrivilegeEscalation': False,
                          'capabilities': {'drop': ['ALL']}}
    def pod(name, role, command, volumes, mounts, annotations=None):
        return {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': name, 'namespace': namespace,
                'labels': {'fixture-role': role}, 'annotations': annotations or {}},
                'spec': {'automountServiceAccountToken': False, 'runtimeClassName': 'ai-sdlc-browser',
                         'restartPolicy': 'Never', 'activeDeadlineSeconds': 300 if role == 'server' else 30,
                         'terminationGracePeriodSeconds': 1, 'securityContext': security,
                         'dnsPolicy': 'None', 'dnsConfig': {'nameservers': ['127.0.0.1']},
                         'volumes': volumes, 'containers': [{'name': role, 'image': image,
                         'imagePullPolicy': 'IfNotPresent', **({'command': command} if command else {}),
                         'securityContext': container_security, 'volumeMounts': mounts,
                         'resources': {'requests': {'cpu': '100m', 'memory': '256Mi'},
                                       'limits': {'cpu': '2', 'memory': '1Gi'}}}]}}
    try:
        uid = create({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
                      'labels': {'pod-security.kubernetes.io/enforce': 'restricted',
                                 'ai-sdlc.browser-fixture': namespace}}})['metadata']['uid']
        create({'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy',
                'metadata': {'name': 'deny-all', 'namespace': namespace},
                'spec': {'podSelector': {}, 'policyTypes': ['Ingress', 'Egress']}})
        policies = {}
        for role, direction, other in [('browser', 'Egress', 'server'), ('server', 'Ingress', 'browser')]:
            policy = {'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy',
                    'metadata': {'name': role, 'namespace': namespace},
                    'spec': {'podSelector': {'matchLabels': {'fixture-role': role}}, 'policyTypes': [direction],
                             direction.lower(): [{('to' if direction == 'Egress' else 'from'): [
                                 {'podSelector': {'matchLabels': {'fixture-role': other}}}],
                                 'ports': [{'protocol': 'TCP', 'port': 8443}]}]}}
            policies[role] = (policy, create(policy)['metadata']['uid'])
        create({'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': 'server-code', 'namespace': namespace},
                'immutable': True, 'data': {'server.py': Path(__file__).with_name('fixtures').joinpath('browser/server.py').read_text()}})
        with tempfile.TemporaryDirectory(prefix='ai-sdlc-browser-tls-') as directory:
            root = Path(directory)
            def openssl(args):
                subprocess.run(['openssl']+args, cwd=root, capture_output=True, check=True, timeout=10)
                for path in root.iterdir():
                    path.chmod(0o600)
            for ca in ('ca', 'wrong-ca'):
                openssl(['req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256', '-nodes',
                         '-days', '1', '-subj', '/CN=synthetic-browser-'+ca, '-keyout', ca+'.key', '-out', ca+'.crt',
                         '-addext', 'basicConstraints=critical,CA:TRUE', '-addext', 'keyUsage=critical,keyCertSign,cRLSign'])
            openssl(['req', '-new', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256', '-nodes',
                     '-subj', '/CN=saferus-console', '-keyout', 'tls.key', '-out', 'tls.csr'])
            (root/'tls.ext').write_text('basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:saferus-console\n')
            openssl(['x509', '-req', '-in', 'tls.csr', '-CA', 'ca.crt', '-CAkey', 'ca.key', '-CAcreateserial',
                     '-days', '1', '-extfile', 'tls.ext', '-out', 'tls.crt'])
            create({'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'fixture-tls', 'namespace': namespace},
                    'immutable': True, 'type': 'kubernetes.io/tls',
                    'data': {key: base64.b64encode((root/key).read_bytes()).decode() for key in ('tls.crt', 'tls.key')}})
            for ca in ('ca', 'wrong-ca'):
                create({'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': ca, 'namespace': namespace},
                        'immutable': True, 'data': {'ca.crt': (root/(ca+'.crt')).read_text()}})
        scenarios = [('baseline', 'baseline-seconds', 'ca', True), ('readable', 'readable-duration', 'ca', True),
                     ('wrong-ttl', 'baseline-seconds', 'ca', False), ('api-failure', 'baseline-seconds', 'ca', False),
                     ('extra-response', 'baseline-seconds', 'ca', False), ('external-request', 'baseline-seconds', 'ca', False),
                     ('baseline', 'baseline-seconds', 'wrong-ca', False)]
        for index, (mode, expectation, ca, success) in enumerate(scenarios):
            server = 'server-'+str(index)
            server_pod = pod(server, 'server', ['python', '/fixture/server.py', mode],
                [{'name': 'code', 'configMap': {'name': 'server-code'}},
                 {'name': 'tls', 'secret': {'secretName': 'fixture-tls', 'defaultMode': 288}}],
                [{'name': 'code', 'mountPath': '/fixture', 'readOnly': True}, {'name': 'tls', 'mountPath': '/tls', 'readOnly': True}])
            server_pod['metadata']['labels']['case'] = str(index)
            server_pod['spec']['containers'][0]['readinessProbe'] = {'tcpSocket': {'port': 8443}, 'periodSeconds': 1}
            server_uid = create(server_pod)['metadata']['uid']
            service = create({'apiVersion': 'v1', 'kind': 'Service', 'metadata': {'name': server, 'namespace': namespace},
                              'spec': {'selector': {'case': str(index)}, 'ports': [{'port': 8443, 'targetPort': 8443}]}})
            try:
                call(['wait', '-n', namespace, '--for=condition=Ready', 'pod/'+server, '--timeout=40s'])
            except ValueError:
                state = get('pod', server, namespace)['status']
                print(json.dumps({'fixtureServerPhase': state.get('phase'), 'containerState':
                                  [item.get('state') for item in state.get('containerStatuses', [])]}), flush=True)
                print(call(['logs', server, '-n', namespace, '--limit-bytes=2048']), flush=True)
                raise
            if get('pod', server, namespace)['metadata']['uid'] != server_uid:
                raise ValueError('server replaced')
            now = int(time.time()*1000)
            request = {'version': 'ai-sdlc.saferus-browser-request/v1', 'requestID': 'browser-'+secrets.token_hex(32),
                       'targetSHA256': 'a'*64, 'snapshotSHA256': 'b'*64, 'imageDigest': 'sha256:'+'c'*64,
                       'issuedAt': now, 'expiresAt': now+30000, 'expectation': expectation}
            raw = json.dumps(request, separators=(',', ':'))
            browser_name = 'browser-'+str(index)
            browser_pod = pod(browser_name, 'browser', None,
                [{'name': 'work', 'emptyDir': {'medium': 'Memory', 'sizeLimit': '512Mi'}},
                 {'name': 'input', 'projected': {'defaultMode': 292, 'sources': [
                    {'configMap': {'name': ca}}, {'downwardAPI': {'items': [{'path': 'request.json',
                     'fieldRef': {'fieldPath': "metadata.annotations['ai-sdlc.io/browser-request']"}}]}}]}}],
                [{'name': 'work', 'mountPath': '/work'}, {'name': 'input', 'mountPath': '/input', 'readOnly': True}],
                {'ai-sdlc.io/browser-request': raw})
            browser_pod['spec']['hostAliases'] = [{'ip': service['spec']['clusterIP'], 'hostnames': ['saferus-console']}]
            browser_uid = create(browser_pod)['metadata']['uid']
            began = time.monotonic()
            while time.monotonic()-began < 45:
                observed = get('pod', browser_name, namespace)
                if observed['metadata']['uid'] != browser_uid:
                    raise ValueError('browser replaced')
                if observed['status'].get('phase') in ('Succeeded', 'Failed'):
                    break
                time.sleep(.5)
            else:
                raise ValueError('browser did not terminate')
            result = json.loads(call(['logs', browser_name, '-n', namespace, '--limit-bytes=4097']))
            status = observed['status']['containerStatuses'][0]
            binding = browser_image.verify(image.split('@')[1], status['imageID'].split('@')[-1], read_image)
            if 'imageBinding' in report and report['imageBinding'] != binding:
                raise ValueError('Browser runtime image binding changed')
            report['imageBinding'] = binding
            passed = observed['status']['phase'] == 'Succeeded' and status['state']['terminated']['exitCode'] == 0 and result['status'] == 'passed'
            if passed != success or (success and result.get('requestSHA256') != hashlib.sha256(raw.encode()).hexdigest()):
                raise ValueError('browser contract outcome differs: '+mode+'/'+ca+'/'+str(result.get('boundary')))
            if not success and (result.get('status') != 'failed' or result.get('reason') != 'closed-browser-check-failed'):
                raise ValueError('negative case did not reject through the component')
            report['scenarios'].append({'mode': mode, 'ca': ca, 'expectedPass': success, 'observedPass': passed,
                                        'elapsedSeconds': round(time.monotonic()-began, 3), 'boundary': result.get('boundary')})
            print(json.dumps(report['scenarios'][-1]), flush=True)
            if index == 0:
                def probe(name, allowed):
                    code = '''import socket,sys,time
expected=sys.argv[2]=='allowed'; consecutive=0
for _ in range(8):
 try:
  connection=socket.create_connection((sys.argv[1],8443),timeout=.5);connection.close();connected=True
 except OSError: connected=False
 consecutive=consecutive+1 if connected==expected else 0
 if consecutive==2:
  print('probe:'+sys.argv[2],flush=True);sys.exit(0)
 time.sleep(.25)
sys.exit(2)
'''
                    resource = pod(name, 'browser', ['python', '-c', code, service['spec']['clusterIP'],
                                   'allowed' if allowed else 'denied'], [], [])
                    original = create(resource)['metadata']['uid']
                    until = time.monotonic()+25
                    while time.monotonic() < until:
                        state = get('pod', name, namespace)
                        if state['metadata']['uid'] != original:
                            raise ValueError('network probe replaced')
                        if state['status'].get('phase') in ('Succeeded', 'Failed'):
                            break
                        time.sleep(.5)
                    logs = call(['logs', name, '-n', namespace, '--limit-bytes=128']).strip()
                    if state['status'].get('phase') != 'Succeeded' or logs != 'probe:'+('allowed' if allowed else 'denied'):
                        raise ValueError('actual browser RuntimeClass network enforcement differs')
                probe('network-before', True)
                for role in ('browser', 'server'):
                    resource, policy_uid = policies[role]
                    call(['delete', '--raw', '/apis/networking.k8s.io/v1/namespaces/'+namespace+'/networkpolicies/'+role, '-f', '-'],
                         {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': policy_uid}})
                    probe('network-denied-'+role, False)
                    policies[role] = (resource, create(resource)['metadata']['uid'])
                    probe('network-restored-'+role, True)
                    original_server = get('pod', server, namespace)
                    if original_server['metadata']['uid'] != server_uid or not any(c['type'] == 'Ready' and c['status'] == 'True' for c in original_server['status'].get('conditions', [])):
                        raise ValueError('network negative depended on unavailable server')
                report['networkPolicyValidated'] = {'runtimeClass': 'ai-sdlc-browser', 'ingress': True,
                                                    'egress': True, 'originalLiveEndpoint': True}
        report['status'] = 'passed'
    finally:
        if uid:
            # Cleanup has its own finite scope and the original namespace UID.
            deadline = time.monotonic()+75
            cleanup_namespace(raw_call, namespace, uid)
            report['cleanupComplete'] = True
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('kubectl', 'kubeconfig', 'image', 'cluster-uid', 'report'):
        parser.add_argument('--'+name, required=True)
    parser.add_argument('--provider', choices=('podman', 'docker'), default='podman')
    args = parser.parse_args()
    report = verify([args.kubectl, '--kubeconfig', args.kubeconfig, '--request-timeout=10s'], args.image, args.cluster_uid, args.provider)
    Path(args.report).write_text(json.dumps(report, indent=2)+'\n')
    print(json.dumps(report))


if __name__ == '__main__':
    main()
