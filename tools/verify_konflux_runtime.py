#!/usr/bin/env python3
"""Run a previously verified Homedir image in a disposable namespace with an egress-deny policy."""
import argparse
import ipaddress
import json
from pathlib import Path
import re
import resource
import secrets
import subprocess
import time

import konflux_build_fixture as fixture
import konflux_fixture_guard as guard
import network_probe
import supply_chain_install

ROOT = Path(__file__).resolve().parents[1]


class HTTPProbeError(ValueError):
    pass


def verified_image(report):
    image = report.get('image', '')
    if (not re.fullmatch(r'registry-service.kind-registry/ai-sdlc-fixtures/homedir@sha256:[a-f0-9]{64}', image)
            or report.get('sourceSHA') != fixture.INPUTS['sourceSHA']
            or report.get('status') != 'isolated-signatures-and-identity-verified'
            or any(report.get(key) is not True for key in ('imageSignatureVerified', 'attestationSignatureVerified', 'signedIdentityBound', 'wrongKeyRejected', 'missingArtifactRejected'))):
        raise ValueError('A successful trusted isolated provenance report is required')
    return image


def runtime_identity(pod, uid, image, container_id=None):
    metadata, spec = pod['metadata'], pod['spec']
    if metadata['uid'] != uid or metadata.get('deletionTimestamp') or len(spec['containers']) != 1 or spec['containers'][0]['image'] != image:
        raise ValueError('Runtime Pod or image identity changed')
    if spec.get('initContainers') or spec.get('ephemeralContainers'):
        raise ValueError('Unexpected application container')
    statuses = pod['status'].get('containerStatuses', [])
    if len(statuses) != 1:
        raise ValueError('Incomplete application container identity')
    status = statuses[0]
    if (status['name'] != 'homedir' or status['restartCount'] or status['imageID'] != image
            or 'running' not in status['state'] or not status.get('containerID')
            or (container_id is not None and status['containerID'] != container_id)):
        raise ValueError('Runtime image or process identity changed')
    if spec.get('automountServiceAccountToken') is not False or any(v.get('secret') or v.get('projected') for v in spec.get('volumes', [])):
        raise ValueError('Application unexpectedly received credentials')
    return status['containerID']


def cleanup_runtime_namespace(call, namespace, uid, output, report, save):
    report['cleanupComplete'] = False; save()
    try:
        try:
            logs = call(['logs', '-n', namespace, 'homedir', '--tail=150', '--limit-bytes=32768'], check=False, limit=10)
            (output/'application.log').write_text(logs[-32768:])
        except (ValueError, OSError, subprocess.SubprocessError):
            report['logCollectionFailed'] = True
        network_probe.cleanup_namespace(call, namespace, uid)
        report['cleanupComplete'] = True; save()
    except BaseException as exc:
        report['cleanupFailure'] = str(exc)[:1500]; save(); raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cluster', default='ai-sdlc-konflux')
    parser.add_argument('--provenance-report', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    if not re.fullmatch(r'[a-z][a-z0-9-]{0,35}', args.cluster):
        raise ValueError('Invalid owned cluster name')
    image = verified_image(json.loads(args.provenance_report.read_text()))
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    state = Path.home()/'.local/state/ai-sdlc'/args.cluster
    ownership = json.loads((state/'konflux-ownership.json').read_text())
    if ownership['scope'] != 'isolated-konflux' or ownership['cluster'] != args.cluster:
        raise ValueError('Unexpected cluster ownership')
    provider = ownership['provider']; node = args.cluster+'-control-plane'
    kubectl = [str(Path.home()/'.cache/ai-sdlc/tools/kubectl-1.35.8'), '--kubeconfig', str(state/'kubeconfig')]
    deadline = supply_chain_install.Deadline(600)
    namespace = 'ai-sdlc-image-smoke-'+secrets.token_hex(6)
    uid = None
    report = {'version': 1, 'image': image, 'namespace': namespace, 'status': 'running',
              'autonomousE2E': False, 'releaseAuthorized': False, 'productionReadinessAccepted': False,
              'profile': 'prod; OIDC disabled; bundled quests; synthetic session configuration; external egress-deny policy configured',
              'networkConformance': 'not independently probed by this runtime smoke'}
    report['harnessCommit'] = deadline.run(['git', '-C', str(ROOT), 'rev-parse', 'HEAD'], capture_output=True, limit=10).stdout.strip()
    report['sourceDirtyAtStart'] = bool(deadline.run(['git', '-C', str(ROOT), 'status', '--porcelain'], capture_output=True, limit=10).stdout)

    def save():
        (args.output/'report.json').write_text(json.dumps(report, indent=2)+'\n')

    def call(arguments, value=None, check=True, limit=90):
        result = deadline.run(kubectl+arguments, input=json.dumps(value) if value is not None else None,
                              capture_output=True, check=False, limit=limit)
        if check and result.returncode:
            raise ValueError('Runtime fixture Kubernetes call failed: '+str(arguments[:3])+' '+result.stderr[-1000:])
        return result.stdout

    def get(kind, name, ns=namespace):
        return json.loads(call(['get', kind, name, '-n', ns, '-o', 'json']))

    def create(value):
        return json.loads(call(['create', '-f', '-', '-o', 'json'], value))

    def policy(name, selector, ingress=None, egress=None):
        spec = {'podSelector': {'matchLabels': selector}, 'policyTypes': []}
        for key, value in [('ingress', ingress), ('egress', egress)]:
            if value is not None:
                spec[key] = value; spec['policyTypes'].append(key.title())
        create({'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy',
                'metadata': {'name': name, 'namespace': namespace}, 'spec': spec})

    def assert_app():
        pod = get('pod', 'homedir')
        report['containerID'] = runtime_identity(pod, report['podUID'], image, report.get('containerID'))
        return pod

    try:
        if get('namespace', 'kube-system', 'default')['metadata']['uid'] != ownership['clusterUID']:
            raise ValueError('Cluster identity changed')
        node_info = json.loads(deadline.run([provider, 'inspect', node], capture_output=True, limit=15).stdout)[0]
        if node_info['Config']['Labels'].get('io.x-k8s.kind.cluster') != args.cluster:
            raise ValueError('Unexpected kind node')
        node_id = node_info['Id']; report['nodeID'] = node_id
        uid = create({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace,
            'labels': {'app.kubernetes.io/part-of': 'ai-sdlc-image-smoke', 'pod-security.kubernetes.io/enforce': 'restricted'}}})['metadata']['uid']
        report['namespaceUID'] = uid; save()
        policy('deny', {}, [], [])
        policy('pull', {'role': 'pull'}, egress=[fixture.PUBLIC_EGRESS[0], fixture.REGISTRY_EGRESS])
        policy('app', {'app': 'homedir'}, ingress=[{'from': [{'podSelector': {'matchLabels': {'probe': 'client'}}}], 'ports': [{'protocol': 'TCP', 'port': 8080}]}])
        policy('client', {'probe': 'client'}, egress=[{'to': [{'podSelector': {'matchLabels': {'app': 'homedir'}}}], 'ports': [{'protocol': 'TCP', 'port': 8080}]}])
        secret = get('secret', 'internal-registry-client-auth', 'kind-registry')
        create({'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'registry-auth', 'namespace': namespace},
                'type': 'kubernetes.io/dockerconfigjson', 'data': {'.dockerconfigjson': secret['data']['.dockerconfigjson']}})
        puller = network_probe.pod(namespace, 'puller', fixture.INPUTS['cosignImage'])
        puller['metadata']['labels']['role'] = 'pull'
        puller['spec']['activeDeadlineSeconds'] = 540
        puller['spec']['securityContext']['fsGroup'] = 1000
        container = puller['spec']['containers'][0]
        container['command'] = ['/bin/sh', '-c', 'sleep 540']
        container['resources'] = {'requests': {'cpu': '100m', 'memory': '128Mi'}, 'limits': {'cpu': '2', 'memory': '1Gi'}}
        container['env'] = [{'name': 'HOME', 'value': '/work'}, {'name': 'TMPDIR', 'value': '/work'}, {'name': 'SSL_CERT_FILE', 'value': '/ca/ca-bundle.crt'}]
        container['volumeMounts'] = [{'name': name, 'mountPath': '/'+name, 'readOnly': name != 'work'} for name in ('auth', 'ca', 'work')]
        puller['spec']['volumes'] = [
            {'name': 'auth', 'secret': {'secretName': 'registry-auth', 'items': [{'key': '.dockerconfigjson', 'path': 'config.json'}]}},
            {'name': 'ca', 'configMap': {'name': 'trusted-ca'}},
            {'name': 'work', 'emptyDir': {'medium': 'Memory', 'sizeLimit': '512Mi'}}]
        pull_uid = create(puller)['metadata']['uid']
        call(['wait', '-n', namespace, '--for=condition=Ready', 'pod/puller', '--timeout=90s'], limit=100)
        call(['exec', '-n', namespace, 'puller', '--', '/usr/bin/skopeo', '--tmpdir', '/work', 'copy', '--preserve-digests',
              '--src-authfile', '/auth/config.json', '--src-cert-dir', '/ca',
              'docker://'+image, 'oci-archive:/work/image.oci:homedir'], limit=120)
        if get('pod', 'puller')['metadata']['uid'] != pull_uid:
            raise ValueError('Image exporter Pod was replaced')
        archive = args.output/'image.oci'
        def bound_output():
            resource.setrlimit(resource.RLIMIT_FSIZE, (512 << 20, 512 << 20))
        with archive.open('xb') as output:
            deadline.run(kubectl+['exec', '-n', namespace, 'puller', '--', '/bin/cat', '/work/image.oci'],
                         stdout=output, stderr=subprocess.PIPE, preexec_fn=bound_output, limit=120)
        report['archiveSHA256'] = guard.validate_oci_archive(archive, image)
        with archive.open('rb') as stream:
            guard.owned_node_exec(deadline.run, provider, node, node_id,
                ['ctr', '-n', 'k8s.io', 'images', 'import', '--local', '--digests', '--base-name', image.split('@')[0], '-'], stdin=stream)
        observed = json.loads(guard.owned_node_exec(deadline.run, provider, node, node_id, ['crictl', 'inspecti', image]).stdout)
        if image not in observed['status'].get('repoDigests', []):
            raise ValueError('Imported image identity mismatch')
        report['importedImageID'] = observed['status']['id']
        call(['delete', '--raw', '/api/v1/namespaces/'+namespace+'/pods/puller', '-f', '-'],
             {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': pull_uid}})
        call(['wait', '-n', namespace, '--for=delete', 'pod/puller', '--timeout=30s'])
        call(['delete', 'secret', 'registry-auth', '-n', namespace])
        app = {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': 'homedir', 'namespace': namespace, 'labels': {'app': 'homedir'}},
            'spec': {'automountServiceAccountToken': False, 'restartPolicy': 'Never', 'activeDeadlineSeconds': 240, 'terminationGracePeriodSeconds': 2,
            'securityContext': {'runAsNonRoot': True, 'runAsUser': 185, 'runAsGroup': 185, 'fsGroup': 185, 'seccompProfile': {'type': 'RuntimeDefault'}},
            'containers': [{'name': 'homedir', 'image': image, 'imagePullPolicy': 'Never',
                'securityContext': {'readOnlyRootFilesystem': True, 'allowPrivilegeEscalation': False, 'capabilities': {'drop': ['ALL']}},
                'resources': {'requests': {'cpu': '100m', 'memory': '256Mi'}, 'limits': {'cpu': '2', 'memory': '1Gi'}},
                'env': [{'name': 'QUARKUS_OIDC_TENANT_ENABLED', 'value': 'false'}, {'name': 'QUESTS_CONTENT_URL', 'value': ''}, {'name': 'OIDC_CLIENT_SECRET', 'value': 'disabled-isolated-fixture'},
                        {'name': 'SESSION_KEY', 'value': secrets.token_hex(32)}, {'name': 'HOMEDIR_DATA_DIR', 'value': '/data'}],
                'volumeMounts': [{'name': 'tmp', 'mountPath': '/tmp'}, {'name': 'data', 'mountPath': '/data'}]}],
            'volumes': [{'name': 'tmp', 'emptyDir': {'medium': 'Memory', 'sizeLimit': '64Mi'}}, {'name': 'data', 'emptyDir': {'medium': 'Memory', 'sizeLimit': '128Mi'}}]}}
        report['podUID'] = create(app)['metadata']['uid']; save()
        lock = json.loads((ROOT/'runtime/k8s/deploy/local/images.lock.json').read_text())
        probe = network_probe.pod(namespace, 'client', lock['image']['repository']+'@'+lock['components']['worker']['digest'])
        probe_uid = create(probe)['metadata']['uid']
        call(['wait', '-n', namespace, '--for=condition=Ready', 'pod/homedir', 'pod/client', '--timeout=90s'], limit=100)
        pod = assert_app(); address = str(ipaddress.ip_address(pod['status']['podIP']))
        if ':' in address: raise ValueError('IPv6 runtime fixture not reviewed')
        report['imageID'] = pod['status']['containerStatuses'][0]['imageID']
        request_script = "const start=performance.now();fetch(process.argv[1],{signal:AbortSignal.timeout(20000),redirect:'error'}).then(async r=>{let body='';for await(const chunk of r.body){body+=Buffer.from(chunk).toString();if(body.length>262144)throw Error('body limit')}console.log(JSON.stringify({status:r.status,milliseconds:performance.now()-start,body,headers:Object.fromEntries(r.headers)}))}).catch(e=>{console.error(e.name);process.exit(1)})"
        def request(route):
            if get('pod', 'client')['metadata']['uid'] != probe_uid: raise ValueError('Runtime probe was replaced')
            result = deadline.run(kubectl+['exec', '-n', namespace, 'client', '--', 'node', '-e', request_script, 'http://'+address+':8080'+route], capture_output=True, check=False, limit=25)
            if result.returncode: raise HTTPProbeError(result.stderr[-500:].strip())
            return json.loads(result.stdout)
        start = time.monotonic()
        while True:
            try:
                home = request('/')
                if home['status'] == 200: break
            except HTTPProbeError:
                if time.monotonic()-start >= 60: raise
            if time.monotonic()-start >= 60: raise ValueError('Home page did not become ready')
            time.sleep(1)
        if '<html' not in home['body'].lower() or home['headers'].get('x-content-type-options') != 'nosniff' or home['headers'].get('x-frame-options') != 'DENY':
            raise ValueError('Home response or security headers differ')
        report['home'] = {'status': home['status'], 'bytes': len(home['body']), 'milliseconds': home['milliseconds'], 'headers': home['headers']}
        (args.output/'home.html').write_text(home['body'])
        report['requests'] = []
        for _ in range(10):
            response = request('/')
            if response['status'] != 200: raise ValueError('Repeated home request failed')
            report['requests'].append(response['milliseconds'])
        report['health'] = {}
        for route in ('/health', '/health/ready'):
            try:
                report['health'][route] = request(route)
            except HTTPProbeError as exc:
                report['health'][route] = {'status': 'request-failed', 'deadlineSeconds': 20, 'reason': str(exc)}
            save()
        call(['exec', '-n', namespace, 'homedir', '--', '/bin/sh', '-c',
              'test -d /data && test -w /data && test "$(id -u)" = 185 && test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token'])
        report['writableDataDirectory'] = True
        assert_app()
        report['status'] = 'isolated-home-and-writable-data-directory-verified'; save()
    except BaseException as exc:
        report['status'] = 'failed'; report['failure'] = str(exc)[:1500]; save(); raise
    finally:
        deadline = supply_chain_install.Deadline(90)
        if uid:
            cleanup_runtime_namespace(call, namespace, uid, args.output, report, save)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
