#!/usr/bin/env python3
"""Verify model Secret refresh replaces a Pod and its private authentication copy.

Uses fake authentication in an owned disposable namespace; never calls a model.
"""
import argparse
import json
import secrets
import subprocess
import time

import network_probe
from verify_workspace_bundle import wire


def verify(kubectl, image):
    namespace = 'ai-sdlc-authcheck-' + secrets.token_hex(6)
    namespace_uid = None
    token = 'fixture-client-' + 'x' * 48
    def command(args, value=None, timeout=60):
        result = subprocess.run(kubectl + args, input=wire(value) if isinstance(value, dict) else value,
                                capture_output=True, timeout=timeout)
        if result.returncode:
            raise ValueError('Credential fixture Kubernetes operation failed')
        return result.stdout
    try:
        created = json.loads(command(['create', '-f', '-', '-o', 'json'], {
            'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
            'labels': {'pod-security.kubernetes.io/enforce': 'restricted'}}}))
        namespace_uid = created['metadata']['uid']
        secret = json.loads(command(['create', '-f', '-', '-o', 'json'], {
            'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'model-auth', 'namespace': namespace},
            'stringData': {'auth.json': json.dumps({'fixture_identity': 'A'})}}))
        revision = secret['metadata']['resourceVersion']
        deployment = {'apiVersion': 'apps/v1', 'kind': 'Deployment', 'metadata': {'name': 'gateway', 'namespace': namespace},
          'spec': {'replicas': 1, 'selector': {'matchLabels': {'app': 'gateway'}},
            'template': {'metadata': {'labels': {'app': 'gateway'}, 'annotations': {'ai-sdlc.io/model-credential-revision': revision}},
             'spec': {'automountServiceAccountToken': False, 'securityContext': {'runAsNonRoot': True, 'runAsUser': 1000, 'runAsGroup': 1000, 'fsGroup': 1000, 'seccompProfile': {'type': 'RuntimeDefault'}},
              'containers': [{'name': 'gateway', 'image': image, 'imagePullPolicy': 'Never',
                'command': ['python3', '/opt/ai-sdlc/model/codex_gateway.py'],
                'env': [{'name': 'CODEX_WRAPPER_TOKEN', 'value': token}, {'name': 'TMPDIR', 'value': '/model-home/tmp'}, {'name': 'PYTHONDONTWRITEBYTECODE', 'value': '1'}],
                'securityContext': {'allowPrivilegeEscalation': False, 'readOnlyRootFilesystem': True, 'capabilities': {'drop': ['ALL']}},
                'resources': {'requests': {'cpu': '50m', 'memory': '64Mi'}, 'limits': {'cpu': '1', 'memory': '768Mi'}},
                'volumeMounts': [{'name': 'auth', 'mountPath': '/var/run/ai-sdlc/codex', 'readOnly': True}, {'name': 'home', 'mountPath': '/model-home'}]}],
              'volumes': [{'name': 'auth', 'secret': {'secretName': 'model-auth'}}, {'name': 'home', 'emptyDir': {'sizeLimit': '128Mi'}}]}}}}
        command(['create', '-f', '-'], deployment)
        observed = []
        for expected in ('A', 'B'):
            if expected == 'B':
                secret = json.loads(command(['-n', namespace, 'patch', 'secret', 'model-auth', '--type=merge', '--patch-file=/dev/stdin', '-o', 'json'], {'stringData': {'auth.json': json.dumps({'fixture_identity': 'B'})}}))
                command(['-n', namespace, 'patch', 'deployment', 'gateway', '--type=merge', '--patch-file=/dev/stdin'], {
                    'spec': {'template': {'metadata': {'annotations': {'ai-sdlc.io/model-credential-revision': secret['metadata']['resourceVersion']}}}}})
            command(['-n', namespace, 'rollout', 'status', 'deployment/gateway', '--timeout=90s'], timeout=100)
            deadline = time.monotonic() + 30
            while True:
                pods = json.loads(command(['-n', namespace, 'get', 'pods', '-l', 'app=gateway', '-o', 'json']))['items']
                current = [p for p in pods if not p['metadata'].get('deletionTimestamp')]
                if len(current) == 1:
                    pod = current[0]
                    code = ('import json,os,urllib.request; '
                            'r=urllib.request.Request("http://127.0.0.1:8787/readyz",headers={"Authorization":"Bearer "+os.environ["CODEX_WRAPPER_TOKEN"]}); '
                            'assert json.load(urllib.request.urlopen(r,timeout=2))["ready"]; '
                            f'assert json.load(open("/model-home/codex/auth.json"))["fixture_identity"]=={expected!r}')
                    result = subprocess.run(kubectl + ['-n', namespace, 'exec', pod['metadata']['name'], '--', 'python3', '-c', code], capture_output=True, timeout=10)
                    if result.returncode == 0:
                        observed.append(pod['metadata']['uid'])
                        break
                if time.monotonic() >= deadline:
                    raise ValueError('New gateway did not use the refreshed fake identity')
                time.sleep(.5)
        if observed[0] == observed[1]:
            raise ValueError('Credential refresh reused the original Pod')
        report = {'scope': 'isolated-model-credential-fixture', 'fakeAuthentication': True,
                  'modelCalls': 0, 'initialIdentityVerified': True, 'refreshedIdentityVerified': True,
                  'newPodUID': True, 'newModelHome': True, 'autonomousE2E': False}
    finally:
        if namespace_uid:
            network_probe.cleanup_namespace(command, namespace, namespace_uid)
    report['ownedNamespaceRemoved'] = True
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('kubectl', 'kubeconfig', 'worker-image'):
        parser.add_argument('--' + name, required=True)
    args = parser.parse_args()
    print(json.dumps(verify([args.kubectl, '--kubeconfig', args.kubeconfig], args.worker_image), indent=2))


if __name__ == '__main__':
    main()
