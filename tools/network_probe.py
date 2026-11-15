"""Disposable network conformance probe; never touches runtime workflow state."""
import json
import secrets
import subprocess
import time


def connection_result(result):
    if (result.returncode, result.stdout.strip()) not in ((0, 'probe:allowed'), (3, 'probe:denied')):
        raise ValueError('Network probe did not execute successfully')
    return result.returncode == 0


def pod(namespace, name, image):
    server = name.startswith('server')
    code = ("require('http').createServer((q,s)=>s.end('network-probe')).listen(8080,'0.0.0.0')"
            if server else 'setInterval(()=>{},1000)')
    container = {'name': 'probe', 'image': image, 'command': ['node', '-e', code],
                 'resources': {'requests': {'cpu': '10m', 'memory': '32Mi'},
                               'limits': {'cpu': '100m', 'memory': '128Mi'}},
                 'securityContext': {'runAsNonRoot': True, 'runAsUser': 1000,
                                     'readOnlyRootFilesystem': True, 'allowPrivilegeEscalation': False,
                                     'capabilities': {'drop': ['ALL']}}}
    if server:
        container['readinessProbe'] = {'tcpSocket': {'port': 8080}, 'periodSeconds': 1}
    return {'apiVersion': 'v1', 'kind': 'Pod',
            'metadata': {'name': name, 'namespace': namespace, 'labels': {'probe': name}},
            'spec': {'automountServiceAccountToken': False, 'restartPolicy': 'Never',
                     'activeDeadlineSeconds': 240, 'terminationGracePeriodSeconds': 1,
                     'securityContext': {'seccompProfile': {'type': 'RuntimeDefault'}},
                     'containers': [container]}}


def policy(namespace, name, selector, direction, rules):
    return {'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy',
            'metadata': {'name': name, 'namespace': namespace},
            'spec': {'podSelector': {'matchLabels': {'probe': selector}},
                     'policyTypes': [direction], direction.lower(): rules}}


def verify(kubectl, image):
    namespace = 'ai-sdlc-netcheck-' + secrets.token_hex(6)
    uid = None

    def call(args, value=None):
        result = subprocess.run([str(v) for v in kubectl] + args, text=True,
                                input=json.dumps(value) if value is not None else None,
                                capture_output=True, timeout=100)
        if result.returncode:
            raise ValueError('Network-policy conformance command failed')
        return result.stdout

    def apply(value):
        call(['apply', '-f', '-'], value)

    def connected(target):
        code = ("const end=(c,m)=>{console.log(m);process.exit(c)};"
                "const q=require('http').get({host:process.argv[1],port:8080,path:'/',timeout:1500},"
                "r=>{let b='';r.on('data',c=>b+=c);r.on('end',()=>end(b==='network-probe'?0:2,'probe:allowed'))});"
                "q.on('timeout',()=>end(3,'probe:denied'));q.on('error',()=>end(3,'probe:denied'));")
        result = subprocess.run([str(v) for v in kubectl] + ['-n', namespace, 'exec', 'client', '--',
                                'node', '-e', code, target], capture_output=True, text=True, timeout=15)
        # Only the probe's own connection failure proves denial. An exec/API
        # failure or a dead client must never be mistaken for network isolation.
        return connection_result(result)

    def expect(target, allowed):
        successes = 0
        for _ in range(12):
            successes = successes + 1 if connected(target) == allowed else 0
            if successes == 2:
                return
            time.sleep(1)
        raise ValueError('Network policy is not enforcing the expected connectivity')

    try:
        created = json.loads(call(['create', '-f', '-', '-o', 'json'], {
            'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
            'labels': {'app.kubernetes.io/part-of': 'ai-sdlc-network-conformance',
                       'pod-security.kubernetes.io/enforce': 'restricted'}}}))
        uid = created['metadata']['uid']
        for name in ('server-a', 'server-b', 'client'):
            apply(pod(namespace, name, image))
        call(['-n', namespace, 'wait', '--for=condition=Ready', 'pods', '--all', '--timeout=90s'])
        servers = [call(['-n', namespace, 'get', 'pod', name, '-o', 'jsonpath={.status.podIP}']).strip()
                   for name in ('server-a', 'server-b')]
        if not all(servers):
            raise ValueError('Network probe addresses are unavailable')
        for target in servers:
            expect(target, True)
        apply(policy(namespace, 'client-egress', 'client', 'Egress', []))
        for target in servers:
            expect(target, False)
        apply(policy(namespace, 'client-egress', 'client', 'Egress', [
            {'to': [{'podSelector': {'matchLabels': {'probe': 'server-a'}}}],
             'ports': [{'protocol': 'TCP', 'port': 8080}]}]))
        expect(servers[0], True)
        expect(servers[1], False)
        apply(policy(namespace, 'server-ingress', 'server-b', 'Ingress', []))
        apply(policy(namespace, 'client-egress', 'client', 'Egress', [{}]))
        expect(servers[0], True)
        expect(servers[1], False)
        # Remove the deny rule and prove the server still works. A dead server
        # cannot yield a successful denial result.
        call(['-n', namespace, 'delete', 'networkpolicy', 'server-ingress'])
        expect(servers[1], True)
    finally:
        if uid:
            current = json.loads(call(['get', 'namespace', namespace, '-o', 'json']))
            if current['metadata']['uid'] != uid:
                raise ValueError('Network probe namespace ownership changed; cleanup refused')
            call(['delete', 'namespace', namespace, '--wait=true', '--timeout=60s'])
    return {'egressDeny': True, 'selectiveEgress': True, 'ingressDeny': True,
            'positiveControls': True, 'temporaryResourcesRemoved': True}
