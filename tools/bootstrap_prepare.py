"""Exclude first-install producers through the existing declarative Helm release.

This is administrative preparation, never workflow pause/resume or delivery
authorization. Any existing workflow history requires the upgrade path instead.
"""
import argparse
import base64
import copy
from contextlib import ExitStack
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
from urllib.parse import urlsplit, urlunsplit
import yaml

import build_runtime_install
import bootstrap_database_forward
import konflux_runtime_install as platform

VERSION = 'ai-sdlc.bootstrap-preparation/v1'
EXECUTION_NAMESPACES = ('ai-sdlc-executors', 'ai-sdlc-verifiers', 'ai-sdlc-reviewers', 'ai-sdlc-builds')


def normalized_template(template):
    """Remove only known API defaults, never credentials, arguments or authority."""
    value = copy.deepcopy(template)
    meta, spec = value.setdefault('metadata', {}), value['spec']
    if meta.get('creationTimestamp') is None:
        meta.pop('creationTimestamp', None)
    for key, default in {'dnsPolicy':'ClusterFirst','restartPolicy':'Always','schedulerName':'default-scheduler',
                         'terminationGracePeriodSeconds':30}.items():
        if spec.get(key) == default:
            spec.pop(key)
    if spec.get('serviceAccount') == spec.get('serviceAccountName'):
        spec.pop('serviceAccount', None)
    for container in spec.get('containers', []):
        for key, default in {'terminationMessagePath':'/dev/termination-log','terminationMessagePolicy':'File'}.items():
            if container.get(key) == default:
                container.pop(key)
        for port in container.get('ports', []):
            if port.get('protocol') == 'TCP':
                port.pop('protocol')
        for probe_key in ('readinessProbe','livenessProbe','startupProbe'):
            probe = container.get(probe_key, {})
            for key, default in {'timeoutSeconds':1,'periodSeconds':10,'successThreshold':1,'failureThreshold':3}.items():
                if probe.get(key) == default:
                    probe.pop(key)
            if probe.get('httpGet', {}).get('scheme') == 'HTTP':
                probe['httpGet'].pop('scheme')
    for volume in spec.get('volumes', []):
        for source in ('secret', 'configMap'):
            if volume.get(source, {}).get('defaultMode') == 420:
                volume[source].pop('defaultMode')
    return value


def runtime_pod_template(pod, node):
    """Compare admitted Pods as well as controller templates, ignoring only
    node assignment and the standard API admission defaults below."""
    spec = copy.deepcopy(pod['spec'])
    if spec.pop('nodeName', None) != node:
        raise ValueError('Runtime producer is not on the original node')
    for key, default in {'enableServiceLinks':True,'priority':0,'preemptionPolicy':'PreemptLowerPriority'}.items():
        if spec.get(key) == default:
            spec.pop(key)
    tolerations = [dict(key=key,operator='Exists',effect='NoExecute',tolerationSeconds=300)
        for key in ('node.kubernetes.io/not-ready','node.kubernetes.io/unreachable')]
    if spec.get('tolerations') == tolerations:
        spec.pop('tolerations')
    meta = {k:copy.deepcopy(pod['metadata'][k]) for k in ('labels','annotations') if k in pod['metadata']}
    meta.get('labels', {}).pop('pod-template-hash', None)
    return normalized_template({'metadata':meta, 'spec':spec})


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


class Preparation:
    def __init__(self, run, kubectl, provider, cluster, state, lock):
        self.run, self.kubectl, self.provider = run, kubectl, provider
        self.cluster, self.state, self.lock = cluster, state, lock
        self.deadline = time.monotonic() + 600
        self.expected = None
        self.templates = None

    def call(self, argv, **kwargs):
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise ValueError('Bootstrap preparation deadline exceeded; keep runtime frozen')
        try:
            return self.run(argv, capture_output=True, timeout=min(kwargs.pop('timeout', 30), remaining), **kwargs)
        except (OSError, subprocess.SubprocessError):
            raise ValueError('Bootstrap preparation operation failed; protected state retained') from None

    def get(self, kind, name=None, namespace=platform.NAMESPACE):
        raw = self.call(self.kubectl + ['get', kind, *([name] if name else []),
            *(['-n', namespace] if namespace else []), '-o', 'json']).stdout
        if len(raw) > 4*1024*1024:
            raise ValueError('Bootstrap inventory exceeded its bound')
        return json.loads(raw)

    @staticmethod
    def identity(value):
        meta = value.get('metadata', {})
        uid = meta.get('uid', '')
        if not re.fullmatch(r'[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}', uid) or meta.get('deletionTimestamp'):
            raise ValueError('Original live resource identity required')
        return uid

    def observe(self, paused=False):
        """Reopen current scope and database custody on every observation."""
        cluster_uid = self.identity(self.get('namespace', 'kube-system', None))
        capability = platform.namespace(self.run, self.kubectl, self.state, cluster_uid, create=False)
        config = self.get('configmap', 'ai-sdlc-config')
        policy = self.get('configmap', 'ai-sdlc-authority')
        data = config.get('data', {})
        inner = json.loads(policy.get('data', {}).get('policy.json', '{}'))
        if (data.get('AI_SDLC_RUNTIME_MODE') != 'maintenance'
                or any(data.get(key, 'false') != 'false' for key in ('AI_SDLC_BUILD', 'AI_SDLC_PROVENANCE', 'AI_SDLC_PUBLICATION', 'AI_SDLC_MERGE'))
                or inner.get('freeze') is not True or inner.get('repository') != 'os-santiago/saferus'
                or inner.get('environment') != 'ai-sdlc-production'
                or policy['metadata'].get('annotations', {}).get('ai-sdlc.io/cluster-uid') != cluster_uid):
            raise ValueError('Initial preparation requires existing frozen Saferus maintenance authority')
        node = json.loads(self.call([self.provider, 'inspect', self.cluster+'-control-plane']).stdout)[0]
        if (node.get('Config', {}).get('Labels', {}).get('io.x-k8s.kind.cluster') != self.cluster
                or node.get('State', {}).get('Status') != 'running'
                or not re.fullmatch('[a-f0-9]{64}', node.get('Id', ''))):
            raise ValueError('Original running kind node required')
        node_object = self.get('node', self.cluster+'-control-plane', None)
        secret = self.get('secret', 'ai-sdlc-postgres-app')
        service = self.get('service', 'ai-sdlc-postgres')
        sts = self.get('statefulset', 'ai-sdlc-postgres')
        pod = self.get('pod', 'ai-sdlc-postgres-0')
        owners = pod['metadata'].get('ownerReferences', [])
        if (not any(o.get('uid') == self.identity(sts) and o.get('controller') is True for o in owners)
                or pod['spec'].get('nodeName') != self.cluster+'-control-plane'
                or not any(c.get('type') == 'Ready' and c.get('status') == 'True' for c in pod.get('status', {}).get('conditions', []))):
            raise ValueError('Original PostgreSQL instance is not ready on the owned node')
        claims = [v['persistentVolumeClaim']['claimName'] for v in pod['spec'].get('volumes', []) if 'persistentVolumeClaim' in v]
        if len(claims) != 1:
            raise ValueError('Single original PostgreSQL volume required')
        pvc = self.get('pvc', claims[0])
        pv = self.get('pv', pvc.get('spec', {}).get('volumeName', ''), None)
        if pvc.get('status', {}).get('phase') != 'Bound' or pv.get('spec', {}).get('claimRef', {}).get('uid') != self.identity(pvc):
            raise ValueError('Original PostgreSQL claim binding unavailable')
        address = pod.get('status', {}).get('podIP', '')
        ipaddress.IPv4Address(address)
        slices = self.get('endpointslices')['items']
        endpoints = [e for s in slices if s['metadata'].get('labels', {}).get('kubernetes.io/service-name') == 'ai-sdlc-postgres' for e in s.get('endpoints', [])]
        if (len(endpoints) != 1 or endpoints[0].get('addresses') != [address]
                or endpoints[0].get('conditions', {}).get('ready') is not True
                or endpoints[0].get('targetRef', {}).get('uid') != self.identity(pod)):
            raise ValueError('PostgreSQL Service does not identify the original primary Pod')
        uri = base64.b64decode(secret.get('data', {}).get('uri', ''), validate=True).decode()
        parsed = urlsplit(uri)
        if (parsed.scheme != 'postgresql' or parsed.hostname != 'ai-sdlc-postgres' or parsed.port != 5432
                or parsed.path != '/ai_sdlc' or parsed.username != 'ai_sdlc' or not parsed.password
                or parsed.query != 'sslmode=disable' or parsed.fragment):
            raise ValueError('Unexpected original bootstrap database connection')
        # Exact Pod address bound through original Service/Endpoint/STS/PVC. The
        # private URI is sent only to the migrator's stdin in the node network.
        private_uri = urlunsplit((parsed.scheme, parsed.netloc.rsplit('@', 1)[0]+'@'+address+':5432', parsed.path, parsed.query, ''))
        expected_names = {'ai-sdlc-'+name for name in ('intake','admission-controller','orchestrator','worker','release-manager','relay')}
        deployments = self.get('deployments')['items']
        if {d['metadata']['name'] for d in deployments} != expected_names:
            raise ValueError('Unexpected delivery controller inventory')
        if self.templates is None or set(self.templates) != expected_names:
            raise ValueError('Reviewed declarative producer inventory required')
        for d in deployments:
            if d['spec'].get('replicas') not in ((0,) if paused else (0, 1)):
                raise ValueError('Delivery producers are not declaratively excluded')
            if paused and d.get('status', {}).get('observedGeneration', 0) < d['metadata'].get('generation', 1):
                raise ValueError('Deployment controller has not observed preparation')
            role = d['metadata']['name'].removeprefix('ai-sdlc-')
            key = {'admission-controller':'admissionController','release-manager':'releaseManager'}.get(role, role)
            containers = d['spec']['template']['spec']['containers']
            if (len(containers) != 1 or containers[0].get('name') != role
                    or containers[0].get('image') != self.lock['image']['repository']+'@'+self.lock['components'][key]['digest']):
                raise ValueError('Installed producer differs from the reviewed image identity')
            if (self.templates is None or normalized_template(d['spec']['template'])
                    != normalized_template(self.templates.get(d['metadata']['name'], {}))):
                raise ValueError('Installed producer template differs from reviewed declarative authority')
        role_secrets = {}
        for role in ('intake','admission-controller','orchestrator','worker','release-manager','relay'):
            resource = self.get('secret', 'ai-sdlc-database-'+role)
            material = {key:base64.b64decode(value, validate=True).decode() for key,value in resource.get('data', {}).items()}
            if (set(material) != {'uri','password'} or not re.fullmatch('[A-Za-z0-9_-]{32,128}',material['password'])
                    or material['uri'] != 'postgresql://aisdlc_'+role.replace('-','_')+':'+material['password']+'@ai-sdlc-postgres:5432/ai_sdlc?sslmode=disable'
                    or resource['metadata'].get('annotations', {}).get('ai-sdlc.io/cluster-uid') != cluster_uid):
                raise ValueError('Producer database custody differs from inspected database')
            role_secrets[role] = {'uid':self.identity(resource), 'sha256':digest(resource['data'])}
        if len(self.get('statefulsets')['items']) != 1 or self.get('cronjobs,daemonsets,hpa')['items']:
            raise ValueError('Unexpected runtime producer inventory')
        jobs = self.get('jobs')['items']
        for job in jobs:
            if (job['metadata'].get('labels', {}).get('app.kubernetes.io/component') != 'migrator'
                    or job.get('status', {}).get('succeeded', 0) != 1 or job.get('status', {}).get('active', 0) != 0
                    or not any(c.get('type') == 'Complete' and c.get('status') == 'True' for c in job.get('status', {}).get('conditions', []))):
                raise ValueError('Administrative migration or unexpected Job is still active')
        namespace_names = {n['metadata']['name'] for n in self.get('namespaces', namespace=None)['items']}
        for ns in EXECUTION_NAMESPACES:
            if ns in namespace_names and self.get('pods,jobs,cronjobs', namespace=ns)['items']:
                raise ValueError('Execution or cleanup workload still exists')
        if 'ai-sdlc-builds' in namespace_names and self.get('pipelineruns,taskruns', namespace='ai-sdlc-builds')['items']:
            raise ValueError('Build execution object still exists')
        deployment_uids = {self.identity(d) for d in deployments}
        replicasets = {}
        for replica in self.get('replicasets')['items']:
            owners = [o for o in replica['metadata'].get('ownerReferences', []) if o.get('controller') is True and o.get('uid') in deployment_uids]
            if len(owners) != 1:
                raise ValueError('Unexpected runtime ReplicaSet')
            replicas = replica.get('spec', {}).get('replicas', 1)
            if paused and replicas != 0:
                raise ValueError('ReplicaSet still authorizes runtime producers')
            if replicas:
                template = copy.deepcopy(replica['spec']['template'])
                template.get('metadata', {}).get('labels', {}).pop('pod-template-hash', None)
                if normalized_template(template) != normalized_template(self.templates[owners[0]['name']]):
                    raise ValueError('Active ReplicaSet differs from reviewed declarative authority')
            replicasets[self.identity(replica)] = owners[0]['name']
        job_uids = {self.identity(j) for j in jobs}
        for current in self.get('pods')['items']:
            if current['metadata']['uid'] == pod['metadata']['uid']:
                continue
            # Completed administrative migration Pods have no running task.
            if (current.get('status', {}).get('phase') == 'Succeeded'
                    and current['metadata'].get('labels', {}).get('app.kubernetes.io/component') == 'migrator'
                    and any(o.get('controller') is True and o.get('uid') in job_uids for o in current['metadata'].get('ownerReferences', []))):
                continue
            if paused:
                raise ValueError('Runtime producer Pod has not terminated')
            owners = [o for o in current['metadata'].get('ownerReferences', []) if o.get('controller') is True and o.get('uid') in replicasets]
            if len(owners) != 1:
                raise ValueError('Unexpected runtime Pod may own unresolved work')
            if runtime_pod_template(current, self.cluster+'-control-plane') != normalized_template(self.templates[replicasets[owners[0]['uid']]]):
                raise ValueError('Admitted producer Pod differs from reviewed declarative authority')
        observed = {'clusterUID': cluster_uid, 'namespaceUID': capability['namespaceUID'],
            'nodeID': node['Id'], 'nodeUID': self.identity(node_object), 'nodeBootID': node_object['status']['nodeInfo']['bootID'],
            'resources': {key:self.identity(value) for key,value in {'config':config,'policy':policy,'databaseSecret':secret,'service':service,'statefulset':sts,'pod':pod,'pvc':pvc,'pv':pv}.items()},
            'policySHA256': digest(policy['data']), 'configSHA256': digest(data), 'databaseSecretSHA256': digest(secret['data']),
            'roleSecrets':role_secrets,
            'deployments': {d['metadata']['name']: {'uid':self.identity(d), 'templateSHA256':digest(d['spec']['template'])} for d in deployments}}
        if self.expected is not None and observed != self.expected:
            raise ValueError('Original preparation identity or frozen authority changed')
        return observed, private_uri

    def inspect(self, paused=False):
        observed, uri = self.observe(paused)
        image = self.lock['image']['repository']+'@'+self.lock['migrator']['digest']
        with bootstrap_database_forward.original_database(self.run,self.kubectl,uri,observed['resources']['pod']) as (forwarded,port):
            result = build_runtime_install.component_call(self.provider, image, 'inspect-empty-bootstrap',
                {'databaseURL':forwarded, 'database':'ai_sdlc'}, host_loopback_port=port)
        if (set(result) != {'version','empty','observedAt','deliveryAuthorized'}
                or result['version'] != 'ai-sdlc.empty-bootstrap-observation/v1'
                or result['empty'] is not True or result['deliveryAuthorized'] is not False):
            raise ValueError('First-install database observation was not successful')
        if self.observe(paused)[0] != observed:
            raise ValueError('Preparation authority changed during database inspection')
        return observed


def main(argv=None):
    import install
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cluster', required=True)
    parser.add_argument('--provider', choices=('podman','docker'), default='podman')
    parser.add_argument('--images', type=Path, required=True)
    args = parser.parse_args(argv)
    if not re.fullmatch('[a-z][a-z0-9-]{0,35}', args.cluster):
        raise ValueError('Invalid local cluster name')
    lock = install.image_lock(args.images)
    state = Path(os.environ.get('XDG_STATE_HOME', Path.home()/'.local/state'))/'ai-sdlc'/args.cluster
    if not state.is_dir() or state.is_symlink():
        raise ValueError('Existing owned frozen installation required')
    binaries = install.tools(Path(os.environ.get('XDG_CACHE_HOME', Path.home()/'.cache'))/'ai-sdlc/tools')
    kubectl = [binaries['kubectl'], '--kubeconfig', str(state/'kubeconfig'), '--context', 'kind-'+args.cluster, '--request-timeout=10s']
    with ExitStack() as stack:
        platform.lock_state(state, stack)
        platform.select_provider(args.provider, state, args.cluster)
        prep = Preparation(install.run, kubectl, args.provider, args.cluster, state, lock)
        path = state/'bootstrap-preparation.json'
        if path.is_symlink():
            raise ValueError('Preparation record cannot use symlinks')
        if path.exists() and path.stat().st_size > 65536:
            raise ValueError('Preparation record exceeds its bound')
        prior = json.loads(path.read_text()) if path.exists() else None
        if prior is not None:
            if (not isinstance(prior, dict) or set(prior) != {'version','imagesSHA256','original','phase'}
                    or prior.get('version') != VERSION or prior.get('imagesSHA256') != digest(lock)
                    or prior.get('phase') not in ('prepared','paused-empty')):
                raise ValueError('Recorded preparation input changed')
            prep.expected = prior['original']
        helm = [binaries['helm'], '--kubeconfig', str(state/'kubeconfig'), '--kube-context', 'kind-'+args.cluster, '-n', platform.NAMESPACE]
        values = json.loads(prep.call(helm+['get','values','ai-sdlc','-o','json']).stdout)
        if (values.get('runtime', {}).get('mode') != 'maintenance'
                or values.get('authority', {}).get('policyJSON') != prep.get('configmap','ai-sdlc-authority')['data']['policy.json']):
            raise ValueError('Helm desired state differs from current frozen authority')
        for key in ('image','migrator'):
            if values.get(key) != lock.get(key):
                raise ValueError('Install the reviewed current image set before preparation')
        for name,pin in lock['components'].items():
            if values.get('components',{}).get(name,{}).get('digest') != pin['digest']:
                raise ValueError('Installed component image differs from reviewed preparation')
        with tempfile.NamedTemporaryFile(mode='w', suffix='.json') as rendered_values:
            json.dump(values, rendered_values); rendered_values.flush()
            rendered = prep.call(helm+['template','ai-sdlc',str(install.CHART),'-f',rendered_values.name]).stdout
        prep.templates = {d['metadata']['name']:d['spec']['template'] for d in yaml.safe_load_all(rendered)
            if d and d.get('kind') == 'Deployment'}
        original = prep.inspect()
        platform.verify_cluster(install.run, kubectl, state, args.cluster, args.provider, original['clusterUID'])
        values.setdefault('runtime', {})['paused'] = True
        prepared = dict(version=VERSION, imagesSHA256=digest(lock), original=original, phase='prepared')
        if prior is None:
            platform.write_record(path, prepared)
        prep.expected = original
        # One normal Helm reconciliation of reviewed declarative desired state.
        # A lost acknowledgement leaves frozen zero-replica intent for reapply.
        prep.inspect()
        with tempfile.NamedTemporaryFile(mode='w', suffix='.json') as value_file:
            json.dump(values, value_file); value_file.flush()
            if prep.deadline - time.monotonic() < 330:
                raise ValueError('Insufficient preparation budget for Helm upgrade; runtime remains frozen')
            prep.call(helm+['upgrade','ai-sdlc',str(install.CHART),'-f',value_file.name,
                '--wait=legacy','--wait-for-jobs','--timeout','5m'], timeout=330)
        until = min(prep.deadline, time.monotonic()+90)
        while True:
            try:
                prep.observe(paused=True)
                break
            except ValueError:
                if time.monotonic() >= until:
                    raise
                time.sleep(1)
        prep.inspect(paused=True)
        prepared['phase'] = 'paused-empty'
        platform.write_record(path, prepared)
        print(json.dumps({'version':VERSION, 'cluster':args.cluster, 'phase':'paused-empty',
            'databaseEmpty':True, 'producersExcluded':True, 'deliveryAuthorized':False}, indent=2))
    return 0
