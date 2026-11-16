#!/usr/bin/env python3
"""Validate registry recovery in an isolated namespace of the owned test cluster.

Uses the existing ai-sdlc-identities-test administrative fixture only. Never
connects to the operational Konflux installation or advances delivery records.
"""
import argparse
import base64
import hashlib
import http.client
import json
from pathlib import Path
import re
import signal
import socket
import subprocess
import time

import install
import validate_provenance_registry as native
from validate_provenance_registry_recovery import Recovery


class CustodyChanged(native.FixtureError):
    pass


class KubernetesRecovery(Recovery):
    def __init__(self, output):
        super().__init__(output)
        self.namespace = 'ai-sdlc-provenance-fixture-' + self.identity[:16]
        self.namespace_uid = None
        self.forward = None
        self.forward_log = None
        self.pod_uid = None
        self.k = None
        self.deadline = time.monotonic() + 900

    def kube(self, argv, value=None, limit=30):
        # stdout can contain fixture-only private material. Never serialize it
        # in a diagnostic. Input travels over stdin, never argv or shell text.
        return self.call(self.k + argv, limit, json.dumps(value).encode() if value is not None else None)

    def read(self, kind, name):
        return json.loads(self.kube(['get', kind, name, '-n', self.namespace, '-o', 'json']))

    def prepare_configuration(self):
        super().prepare_configuration()
        state = Path.home() / '.local/state/ai-sdlc/ai-sdlc-identities-test'
        owner = json.loads((state / 'ownership.json').read_text())
        if owner.get('cluster') != 'ai-sdlc-identities-test' or owner.get('provider') != 'podman':
            raise native.FixtureError('Dedicated administrative fixture ownership required')
        binaries = install.tools(Path.home() / '.cache/ai-sdlc/tools')
        self.k = [binaries['kubectl'], '--kubeconfig', str(state / 'kubeconfig'), '--context', 'kind-ai-sdlc-identities-test']
        system = json.loads(self.kube(['get', 'namespace', 'kube-system', '-o', 'json']))
        if system['metadata']['uid'] != owner.get('clusterUID'):
            raise native.FixtureError('Original fixture cluster changed')
        node = json.loads(self.call(['podman', 'inspect', 'ai-sdlc-identities-test-control-plane']))[0]
        if (not re.fullmatch('[a-f0-9]{64}', node.get('Id', ''))
                or node.get('Config', {}).get('Labels', {}).get('io.x-k8s.kind.cluster') != owner['cluster']):
            raise native.FixtureError('Original fixture node required')
        self.node_id = node['Id']
        self.report['scope'] = 'isolated-kubernetes-registry-recovery-fixture'
        self.report['fixtureClusterUID'] = system['metadata']['uid']

    def fixture_files(self):
        return {name: base64.b64encode((self.private/name).read_bytes()).decode()
                for name in ('config.json', 'htpasswd', 'tls.crt', 'tls.key')}

    def start_registry(self, uid, gid):
        namespace = {'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {
            'name': self.namespace, 'labels': {native.LABEL: self.identity}}}
        created = json.loads(self.kube(['create', '-f', '-', '-o', 'json'], namespace))
        self.namespace_uid = created['metadata']['uid']
        self.kube(['create', '-f', '-'], {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {
            'name': 'registry-fixture', 'namespace': self.namespace}, 'type': 'Opaque', 'data': self.fixture_files()})
        pod = {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': 'registry', 'namespace': self.namespace,
               'labels': {native.LABEL: self.identity}}, 'spec': {'automountServiceAccountToken': False,
               'nodeName': 'ai-sdlc-identities-test-control-plane',
               'securityContext': {'runAsNonRoot': True, 'runAsUser': 10000, 'runAsGroup': 10000,
                                   'fsGroup': 10000, 'seccompProfile': {'type': 'RuntimeDefault'}},
               'containers': [{'name': 'registry', 'image': native.IMAGE, 'args': ['/cfg/config.json'],
                    'securityContext': {'allowPrivilegeEscalation': False, 'readOnlyRootFilesystem': True,
                                        'capabilities': {'drop': ['ALL']}},
                    'resources': {'requests': {'cpu': '20m', 'memory': '32Mi'}, 'limits': {'cpu': '1', 'memory': '256Mi'}},
                    'readinessProbe': {'tcpSocket': {'port': 5000}, 'periodSeconds': 1},
                    'volumeMounts': [{'name': 'config', 'mountPath': '/cfg', 'readOnly': True},
                                     {'name': 'storage', 'mountPath': '/var/lib/zot'}]}],
               'volumes': [{'name': 'config', 'secret': {'secretName': 'registry-fixture', 'defaultMode': 288}},
                           {'name': 'storage', 'emptyDir': {'medium': 'Memory', 'sizeLimit': '128Mi'}}]}}
        self.pod_manifest = pod
        created = json.loads(self.kube(['create', '-f', '-', '-o', 'json'], pod))
        self.pod_uid = created['metadata']['uid']
        self.kube(['wait', '--for=condition=Ready', 'pod/registry', '-n', self.namespace, '--timeout=150s'], limit=155)
        self.storage_id = self.volume_identity()
        with socket.socket() as reserved:
            reserved.bind(('127.0.0.1', 0))
            port = reserved.getsockname()[1]
        self.forward_log = (self.private/'port-forward.log').open('wb')
        self.forward = subprocess.Popen(self.k + ['port-forward', '-n', self.namespace, 'pod/registry',
            str(port)+':5000', '--address=127.0.0.1'], stdout=self.forward_log, stderr=subprocess.STDOUT)
        return '127.0.0.1:' + str(port)

    def guard(self):
        ns = json.loads(self.kube(['get', 'namespace', self.namespace, '-o', 'json']))
        pod = self.read('pod', 'registry')
        if (ns['metadata']['uid'] != self.namespace_uid or ns['metadata']['labels'].get(native.LABEL) != self.identity
                or pod['metadata']['uid'] != self.pod_uid or pod['metadata']['labels'].get(native.LABEL) != self.identity
                or pod['spec']['containers'][0]['image'] != native.IMAGE
                or self.volume_identity() != self.storage_id):
            raise CustodyChanged('Original Pod or volume custody changed')
        return pod

    def volume_identity(self):
        if not re.fullmatch('[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}', self.pod_uid or ''):
            raise native.FixtureError('Original Pod UID required')
        path = '/var/lib/kubelet/pods/'+self.pod_uid+'/volumes/kubernetes.io~empty-dir/storage'
        result = self.call(['podman', 'exec', self.node_id, 'stat', '-c', '%d:%i', path]).strip()
        if not re.fullmatch('[0-9]+:[0-9]+', result):
            raise native.FixtureError('Original emptyDir identity unavailable')
        return result

    def restart(self, request, image_path, endpoint):
        before = self.guard()
        secret = self.read('secret', 'registry-fixture')
        secret['data'] = self.fixture_files()
        self.kube(['replace', '-f', '-'], secret)  # UID/resourceVersion CAS.
        expected = [hashlib.sha256((self.private/n).read_bytes()).hexdigest() for n in ('config.json', 'htpasswd')]
        deadline = time.monotonic() + 130
        while True:
            self.guard()
            raw = self.kube(['exec', '-n', self.namespace, 'registry', '--', '/usr/bin/sha256sum', '/cfg/config.json', '/cfg/htpasswd'])
            if [line.split()[0] for line in raw.splitlines()] == expected:
                break
            if time.monotonic() >= deadline:
                raise native.FixtureError('Kubernetes projected Secret did not converge')
            time.sleep(1)
        original = before['status']['containerStatuses'][0]
        cid = original['containerID']
        if not re.fullmatch('containerd://[a-f0-9]{64}', cid):
            raise native.FixtureError('Original runtime container identity unavailable')
        self.guard()
        # Signal exactly the original runtime task on the owned fixture node.
        # Do not resolve a Pod name into a replacement container at mutation time.
        self.call(['podman', 'exec', self.node_id, 'ctr', '-n', 'k8s.io', 'tasks', 'kill', '--signal', 'SIGTERM', cid.split('://')[1]])
        deadline = time.monotonic() + 100
        while True:
            current = self.guard()['status'].get('containerStatuses', [{}])[0]
            if (current.get('ready') and current.get('containerID') != cid
                    and current.get('restartCount') == original['restartCount'] + 1):
                try:
                    if request('GET', image_path)[0] == 200:
                        break
                except (OSError, http.client.HTTPException):
                    pass
            if time.monotonic() >= deadline:
                raise native.FixtureError('Original Pod did not recover retained artifact')
            time.sleep(1)

    def after_seed(self, request, image_path, endpoint):
        super().after_seed(request, image_path, endpoint)
        self.report['recovery'].update(sameContainer=False, samePod=True, sameEmptyDir=True,
            kubernetesRestartValidated=True, actualSecretProjectionValidated=True)

    def after_verification(self, request, image_path, endpoint):
        self.guard()
        self.kube(['delete', '--raw', '/api/v1/namespaces/'+self.namespace+'/pods/registry', '-f', '-'],
                  {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': self.pod_uid}})
        self.kube(['wait', '--for=delete', 'pod/registry', '-n', self.namespace, '--timeout=45s'], limit=50)
        replacement = json.loads(self.kube(['create', '-f', '-', '-o', 'json'], self.pod_manifest))
        if replacement['metadata']['uid'] == self.pod_uid:
            raise native.FixtureError('Actual replacement Pod required for negative')
        self.kube(['wait', '--for=condition=Ready', 'pod/registry', '-n', self.namespace, '--timeout=60s'], limit=65)
        before = self.read('secret', 'registry-fixture')
        try:
            self.restart(request, image_path, endpoint)
        except CustodyChanged:
            pass
        else:
            raise native.FixtureError('Replacement Pod was accepted by original recovery')
        after = self.read('secret', 'registry-fixture')
        if before['data'] != after['data'] or before['metadata']['resourceVersion'] != after['metadata']['resourceVersion']:
            raise native.FixtureError('Rejected replacement changed credential custody')
        self.report['recovery'].update(actualPodReplacementRejected=True, rejectedReplacementChangedSecret=False)

    def cleanup(self):
        self.deadline = time.monotonic() + 100
        complete = self.k is None
        try:
            if self.forward is not None:
                self.forward.terminate()
                try:
                    self.forward.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    self.forward.kill()
                    self.forward.wait(timeout=5)
            if self.forward_log is not None:
                self.forward_log.close()
            if self.k is not None:
                raw = self.kube(['get', 'namespace', self.namespace, '--ignore-not-found', '-o', 'json'])
                if raw.strip():
                    obj = json.loads(raw)
                    if (obj['metadata']['labels'].get(native.LABEL) != self.identity
                            or self.namespace_uid is not None and obj['metadata']['uid'] != self.namespace_uid):
                        raise native.FixtureError('Refusing cleanup of another namespace')
                    self.kube(['delete', '--raw', '/api/v1/namespaces/'+self.namespace, '-f', '-'],
                              {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': obj['metadata']['uid']}})
                    self.kube(['wait', '--for=delete', 'namespace/'+self.namespace, '--timeout=60s'], limit=65)
                complete = not self.kube(['get', 'namespace', self.namespace, '--ignore-not-found', '-o', 'json']).strip()
        except (OSError, ValueError, subprocess.SubprocessError, native.FixtureError):
            complete = False
        self.report['namespaceRemoved'] = complete
        super().cleanup()
        if not complete:
            self.report['cleanupComplete'] = False


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    output = args.output.absolute()
    if ':' in str(output) or ':' in str(native.ROOT):
        parser.error('Container paths cannot contain colons')
    def interrupted(signum, frame):
        raise native.FixtureError('Fixture interrupted')
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    passed = KubernetesRecovery(output).execute()
    print(('PASS' if passed else 'FAIL') + ': ' + str(output/'report.json'))
    return 0 if passed else 1


if __name__ == '__main__':
    raise SystemExit(main())
