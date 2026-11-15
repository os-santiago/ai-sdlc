#!/usr/bin/env python3
"""Install an isolated, pinned Konflux validation environment through Helm."""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import resource
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
DESIRED = ROOT / 'runtime/k8s/deploy/konflux'
CHART = ROOT / 'runtime/k8s/deploy/helm/ai-sdlc-konflux-local'
DEPENDENCIES = ('cert-manager', 'prometheus-operator-crds', 'trust-manager', 'tekton-operator',
                'tekton-config', 'tekton-chains-rbac', 'admission-policies/policy')
RELEASE = 'ai-sdlc-konflux'
RELEASE_NAMESPACE = 'ai-sdlc-platform'
DOWNLOAD_CODE = '''import pathlib,sys,urllib.request
with urllib.request.urlopen(sys.argv[1], timeout=60) as response:
    raw=response.read(int(sys.argv[3])+1)
if len(raw)>int(sys.argv[3]): raise ValueError('download size bound')
with pathlib.Path(sys.argv[2]).open('xb') as output: output.write(raw)
'''


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def lockfile():
    raw = (DESIRED/'upstream.lock.json').read_bytes()
    lock = json.loads(raw)
    source, operator = lock['source'], lock['operator']
    if (lock['version'] != 1 or not re.fullmatch(r'[a-f0-9]{40}', source['commit'])
            or source['url'] != 'https://codeload.github.com/konflux-ci/konflux-ci/tar.gz/'+source['commit']
            or not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+', lock['release'])
            or operator['url'] != 'https://github.com/konflux-ci/konflux-ci/releases/download/'+lock['release']+'/install.yaml'
            or not re.fullmatch(r'quay.io/konflux-ci/konflux-operator@sha256:[a-f0-9]{64}', operator['image'])
            or not re.fullmatch(r'kindest/node:v[0-9.]+@sha256:[a-f0-9]{64}', lock['nodeImage'])
            or [d['path'] for d in lock['dependencies']] != ['dependencies/'+d for d in DEPENDENCIES]):
        raise ValueError('Unsupported Konflux source lock')
    for asset in [source, operator]+lock['dependencies']:
        if not re.fullmatch(r'[a-f0-9]{64}', asset['sha256']):
            raise ValueError('Missing Konflux artifact digest')
    for dep in lock['dependencies']:
        if type(dep['bytes']) is not int or not 0<dep['bytes']<=2*1024*1024:
            raise ValueError('Invalid dependency manifest bound')
    required = lock['requiredDeployments']
    if (not required or required != sorted(set(required))
            or any(not re.fullmatch(r'[a-z0-9-]+/[a-z0-9-]+', name) for name in required)):
        raise ValueError('Invalid required deployment inventory')
    kind = json.loads((DESIRED/'kind.json').read_text())
    if kind['networking'].get('apiServerAddress')!='127.0.0.1' or len(kind['nodes'])!=1 or kind['nodes'][0].get('image')!=lock['nodeImage'] or kind['nodes'][0].get('extraPortMappings'):
        raise ValueError('Konflux validation must use its pinned isolated local cluster')
    return lock, digest(raw)


class Deadline:
    def __init__(self, seconds):
        self.end = time.monotonic()+seconds

    def remaining(self, maximum=1800):
        remaining = min(maximum, self.end-time.monotonic())
        if remaining<=0:
            raise ValueError('Konflux installation deadline exceeded; owned state is retained for inspection/reapplication')
        return remaining

    def run(self, args, **kwargs):
        return subprocess.run([str(a) for a in args], check=kwargs.pop('check', True),
                              text=True, timeout=self.remaining(kwargs.pop('limit', 600)), **kwargs)


def verified_asset(asset, cache, deadline, bound):
    path = cache/asset['sha256']
    if path.is_symlink():
        raise ValueError('Artifact cache cannot contain symlinks')
    if path.exists():
        if not path.is_file():
            raise ValueError('Artifact cache must contain regular files')
        with path.open('rb') as stream:
            raw = stream.read(bound+1)
        if len(raw)<=bound and digest(raw)==asset['sha256']:
            return raw
    # Socket inactivity timeouts do not bound a slowly progressing response.
    # A child with a wall-clock deadline bounds the entire download instead.
    with tempfile.TemporaryDirectory(prefix='download-',dir=cache) as directory:
        downloaded = Path(directory)/'asset'
        try:
            deadline.run([sys.executable,'-c',DOWNLOAD_CODE,asset['url'],downloaded,str(bound)],
                         stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,limit=60)
        except subprocess.TimeoutExpired:
            raise ValueError('Konflux download exceeded its wall-clock deadline') from None
        except subprocess.CalledProcessError:
            raise ValueError('Konflux download failed or exceeded its size bound') from None
        with downloaded.open('rb') as stream:
            raw=stream.read(bound+1)
    if len(raw)>bound or digest(raw)!=asset['sha256']:
        raise ValueError('Konflux download failed its committed size/digest check')
    with tempfile.NamedTemporaryFile(dir=cache, delete=False) as stream:
        stream.write(raw)
        temporary = Path(stream.name)
    temporary.replace(path)
    return raw


def extract_dependencies(raw, destination, commit):
    prefix = 'konflux-ci-'+commit+'/'
    total, count, archive_bytes, archive_entries = 0, 0, 0, 0
    with tarfile.open(fileobj=io.BytesIO(raw), mode='r:gz') as archive:
        for member in archive:
            archive_entries += 1
            archive_bytes += member.size
            if member.size<0 or archive_entries>20000 or archive_bytes>128*1024*1024:
                raise ValueError('Source archive exceeded traversal bound')
            if not member.name.startswith(prefix+'dependencies/'):
                continue
            relative = PurePosixPath(member.name[len(prefix):])
            if relative.is_absolute() or '..' in relative.parts or not (member.isdir() or member.isfile()):
                raise ValueError('Unsafe dependency archive entry')
            if member.isdir():
                continue
            count += 1
            total += member.size
            if count>4096 or member.size<0 or total>16*1024*1024:
                raise ValueError('Dependency archive exceeded extraction bound')
            target = destination.joinpath(*relative.parts)
            target.parent.mkdir(parents=True, exist_ok=True)
            with target.open('xb') as output, archive.extractfile(member) as source:
                shutil.copyfileobj(source, output)
    if count==0:
        raise ValueError('Dependency archive is empty')


def prepare(lock, cache, binaries, deadline):
    cache.mkdir(mode=0o700, parents=True, exist_ok=True)
    operator = verified_asset(lock['operator'], cache, deadline, 8*1024*1024)
    old, new = lock['operator']['originalImage'].encode(), lock['operator']['image'].encode()
    if operator.count(old)!=1:
        raise ValueError('Unexpected operator image occurrence in reviewed release asset')
    rendered = {}
    with tempfile.TemporaryDirectory(prefix='konflux-source-', dir=cache) as directory:
        source = Path(directory)
        extracted = False
        for dep in lock['dependencies']:
            path = cache/(dep['sha256']+'.yaml')
            if path.is_symlink():
                raise ValueError('Manifest cache cannot contain symlinks')
            if path.exists() and not path.is_file():
                raise ValueError('Manifest cache must contain regular files')
            raw = path.read_bytes() if path.exists() and path.stat().st_size<=dep['bytes'] else b''
            if digest(raw)!=dep['sha256']:
                if not extracted:
                    archive = verified_asset(lock['source'], cache, deadline, 32*1024*1024)
                    extract_dependencies(archive, source, lock['source']['commit'])
                    extracted = True
                with tempfile.TemporaryFile() as output:
                    def bounds():
                        resource.setrlimit(resource.RLIMIT_FSIZE, (2*1024*1024, 2*1024*1024))
                    deadline.run([binaries['kubectl'], 'kustomize', source/dep['path']], stdout=output,
                                 env=dict(os.environ, KUBECONFIG=os.devnull), preexec_fn=bounds, limit=90)
                    output.seek(0)
                    raw = output.read(dep['bytes']+1)
                if len(raw)!=dep['bytes'] or digest(raw)!=dep['sha256']:
                    raise ValueError('Rendered dependency differs from committed lock: '+dep['path'])
                path.write_bytes(raw)
            rendered[dep['path'].removeprefix('dependencies/')] = path
    return operator.replace(old, new), rendered


def prerequisites(lock):
    policy = lock['prerequisites']
    if (os.cpu_count() or 0)<policy['minimumCPUs']:
        raise ValueError('Konflux validation requires at least four CPUs')
    mem = dict(line.split(':',1) for line in Path('/proc/meminfo').read_text().splitlines())
    available = int(mem['MemAvailable'].split()[0])*1024
    if available<policy['minimumAvailableMemoryGiB']*1024**3:
        raise ValueError('Konflux validation requires at least eight GiB available memory; sixteen GiB is recommended')
    return {'cpus':os.cpu_count(), 'availableMemoryBytes':available}


def observe(kubectl, deadline, cluster_uid, source_digest, required_deployments):
    def get(*args):
        return json.loads(deadline.run(kubectl+['get',*args,'-o','json'], capture_output=True, limit=30).stdout)
    konflux = get('konflux','konflux')
    deployments = get('deployments','-A')['items']
    pods = get('pods','-A')['items']
    generation=konflux['metadata'].get('generation')
    ready = generation is not None and not konflux['metadata'].get('deletionTimestamp') and any(
        c['type']=='Ready' and c['status']=='True' and c.get('observedGeneration')==generation
        for c in konflux.get('status',{}).get('conditions',[]))
    def current_deployment(d):
        status=d.get('status',{});desired=d['spec'].get('replicas',1)
        return (desired>0 and d['metadata'].get('generation') is not None and not d['metadata'].get('deletionTimestamp')
                and status.get('observedGeneration')==d['metadata']['generation']
                and all(status.get(field,0)>=desired for field in ('updatedReplicas','readyReplicas','availableReplicas'))
                and status.get('unavailableReplicas',0)==0)
    present = {d['metadata']['namespace']+'/'+d['metadata']['name'] for d in deployments}
    missing = sorted(set(required_deployments)-present)
    ready = ready and not missing and all(current_deployment(d) for d in deployments)
    return {'version':1, 'status':'ready' if ready else 'not-ready', 'clusterUID':cluster_uid,
            'desiredSourceLockSHA256':source_digest, 'scope':'isolated-supply-chain-validation',
            'liveWorkflowAdvanced':False, 'githubAppProvisionedByInstaller':False, 'externalRegistryProvisionedByInstaller':False,
            'provenanceValidated':False, 'slsaLevelClaimed':False, 'konfluxUID':konflux['metadata']['uid'],
            'missingRequiredDeployments':missing,
            'deployments':[{'namespace':d['metadata']['namespace'], 'name':d['metadata']['name'],
                            'generation':d['metadata'].get('generation'),'observedGeneration':d.get('status',{}).get('observedGeneration'),
                            'desired':d['spec'].get('replicas',1),'updated':d.get('status',{}).get('updatedReplicas',0),
                            'ready':d.get('status',{}).get('readyReplicas',0),'available':d.get('status',{}).get('availableReplicas',0)} for d in deployments],
            'containers':[{'namespace':p['metadata']['namespace'],'pod':p['metadata']['name'],
                           'name':c['name'],'imageID':c.get('imageID'),'ready':c.get('ready'),'restarts':c.get('restartCount')}
                          for p in pods for c in p.get('status',{}).get('containerStatuses',[])]}


def existing_konflux(kubectl, deadline):
    # A failed API request is not evidence of absence. Disabling an existing CR
    # on a Helm upgrade would delete it, so every ambiguous observation fails.
    crd = deadline.run(kubectl+['get','crd','konfluxes.konflux.konflux-ci.dev','--ignore-not-found','-o','json'],
                       capture_output=True,limit=20).stdout
    if not crd.strip():
        return False
    raw = deadline.run(kubectl+['get','konflux','konflux','--ignore-not-found','-o','json'],
                       capture_output=True,limit=20).stdout
    if not raw.strip():
        return False
    annotations = json.loads(raw)['metadata'].get('annotations',{})
    if annotations.get('meta.helm.sh/release-name')!=RELEASE or annotations.get('meta.helm.sh/release-namespace')!=RELEASE_NAMESPACE:
        raise ValueError('Existing Konflux resource is not owned by this Helm release')
    return True


def select_provider(requested, marker, cluster):
    record = json.loads(marker.read_text()) if marker.exists() else None
    if record is not None:
        if (set(record)!={'version','cluster','provider','clusterUID','scope'} or record.get('version')!=1
                or record.get('cluster')!=cluster or record.get('provider') not in ('docker','podman')
                or record.get('scope')!='isolated-konflux' or not isinstance(record.get('clusterUID'),str)
                or not re.fullmatch(r'[a-f0-9-]{36}',record['clusterUID'])):
            raise ValueError('Invalid isolated supply-chain ownership record')
        if requested!='auto' and requested!=record['provider']:
            raise ValueError('Requested provider differs from the recorded cluster; refusing to replace its ownership')
        return record['provider']
    return requested if requested!='auto' else next((p for p in ('docker','podman') if shutil.which(p)),None)


def bootstrap_tools(deadline):
    try:
        result=deadline.run([sys.executable,ROOT/'tools/install.py','tools'],capture_output=True,limit=180)
    except subprocess.TimeoutExpired:
        raise ValueError('Tool bootstrap exceeded the installation wall-clock deadline') from None
    return json.loads(result.stdout)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('plan','local','status'))
    parser.add_argument('--cluster', default='ai-sdlc-konflux')
    parser.add_argument('--provider', choices=('auto','docker','podman'), default='auto')
    parser.add_argument('--timeout', type=int, default=1800, help='Overall seconds, maximum 1800')
    args = parser.parse_args(argv)
    if not re.fullmatch(r'[a-z][a-z0-9-]{0,35}',args.cluster) or not 30<=args.timeout<=1800:
        raise ValueError('Invalid isolated cluster name or timeout (30..1800 seconds)')
    lock, source_digest = lockfile()
    if args.action=='plan':
        print(json.dumps({'cluster':args.cluster,'release':lock['release'],'nodeImage':lock['nodeImage'],
            'operatorImage':lock['operator']['image'],'sourceLockSHA256':source_digest,'prerequisites':lock['prerequisites'],
            'chart':str(CHART.relative_to(ROOT)), 'hostPortMappings':[], 'defaultUsers':[],
            'liveWorkflowAdvanced':False,'githubAppConfigured':False,'externalRegistryConfigured':False},indent=2))
        return 0
    import install
    deadline = Deadline(args.timeout)
    state = Path(os.environ.get('XDG_STATE_HOME',Path.home()/'.local/state'))/'ai-sdlc'/args.cluster
    state.mkdir(mode=0o700,parents=True,exist_ok=True)
    marker = state/'konflux-ownership.json'
    kubeconfig = state/'kubeconfig'
    if marker.is_symlink() or kubeconfig.is_symlink():
        raise ValueError('Owned supply-chain state cannot use symlinks')
    provider = select_provider(args.provider,marker,args.cluster)
    if not provider or not shutil.which(provider):
        raise ValueError('Install Docker or Podman before running the isolated supply-chain installer')
    deadline.run([provider,'info'],stdout=subprocess.DEVNULL,limit=30)
    cache = Path(os.environ.get('XDG_CACHE_HOME',Path.home()/'.cache'))/'ai-sdlc'
    binaries = bootstrap_tools(deadline)
    env = dict(os.environ,KIND_EXPERIMENTAL_PROVIDER=provider)
    clusters = deadline.run([binaries['kind'],'get','clusters'],env=env,capture_output=True,limit=30).stdout.splitlines()
    exists = args.cluster in clusters
    if marker.exists() and not exists:
        raise ValueError('Recorded cluster is absent from its provider; refusing to overwrite its identity. Choose a new --cluster.')
    if exists and not marker.exists():
        raise ValueError('Cluster exists without this supply-chain ownership record; choose a new --cluster')
    if args.action=='status' and not exists:
        raise ValueError('The isolated supply-chain cluster does not exist')
    if args.action=='local':
        prerequisites(lock)
        install.host_prerequisites(False)
        operator, dependencies = prepare(lock,cache/('konflux-'+source_digest),binaries,deadline)
    if not exists:
        deadline.run([binaries['kind'],'create','cluster','--name',args.cluster,'--config',DESIRED/'kind.json',
                      '--kubeconfig',kubeconfig,'--wait','180s'],env=env,limit=300)
    deadline.run([binaries['kind'],'export','kubeconfig','--name',args.cluster,'--kubeconfig',kubeconfig],env=env,limit=30)
    kubeconfig.chmod(0o600)
    kubectl = [binaries['kubectl'],'--kubeconfig',str(kubeconfig),'--context','kind-'+args.cluster]
    uid = deadline.run(kubectl+['get','namespace','kube-system','-o','jsonpath={.metadata.uid}'],capture_output=True,limit=30).stdout.strip()
    if not uid:
        raise ValueError('Cannot establish isolated cluster identity')
    expected = {'version':1,'cluster':args.cluster,'provider':provider,'clusterUID':uid,'scope':'isolated-konflux'}
    if exists:
        if json.loads(marker.read_text())!=expected:
            raise ValueError('Isolated cluster ownership differs; refusing adoption')
    else:
        marker.write_text(json.dumps(expected)+'\n')
    if args.action=='status':
        report = observe(kubectl,deadline,uid,source_digest,lock['requiredDeployments'])
        print(json.dumps(report,indent=2))
        return 0 if report['status']=='ready' else 1

    def apply(name):
        print('Applying verified Konflux dependency: '+name,flush=True)
        deadline.run(kubectl+['apply','-f',dependencies[name]],limit=90)

    def wait(args, seconds):
        limit = max(1,int(deadline.remaining(seconds)))
        deadline.run(kubectl+['wait',*args,'--timeout='+str(limit)+'s'],limit=limit+5)

    def exists_resource(args):
        until = min(deadline.end,time.monotonic()+120)
        while time.monotonic()<until:
            if deadline.run(kubectl+['get',*args],capture_output=True,check=False,limit=15).returncode==0:
                return
            time.sleep(min(1,deadline.remaining(1)))
        raise ValueError('Operator did not create its expected resource before the deadline')

    apply('cert-manager')
    wait(['--for=condition=Available','deployment','-l','app.kubernetes.io/instance=cert-manager','-n','cert-manager'],180)
    apply('prometheus-operator-crds')
    apply('trust-manager')
    wait(['--for=condition=Available','deployment/trust-manager','-n','cert-manager'],180)
    apply('tekton-operator')
    wait(['--for=condition=Available','deployment','--all','-n','tekton-operator'],240)
    exists_resource(['tektonconfig','config'])
    wait(['--for=condition=Ready','tektonconfig/config'],360)
    apply('tekton-config')
    wait(['--for=condition=Ready','tektonconfig/config'],180)
    apply('tekton-chains-rbac')
    wait(['--for=condition=complete','job/tekton-chains-signing-secret','-n','tekton-pipelines'],180)
    apply('admission-policies/policy')
    # Dex, the cluster issuer and the internal registry belong to the operator.
    # Deploying legacy Dex here deadlocks readiness before its issuer exists.
    with tempfile.TemporaryDirectory(prefix='konflux-chart-',dir=state) as directory:
        chart = Path(directory)/'chart'
        shutil.copytree(CHART,chart)
        (chart/'files').mkdir()
        (chart/'files/operator.yaml').write_bytes(operator)
        enabled = existing_konflux(kubectl,deadline)
        helm = [binaries['helm'],'upgrade','--install',RELEASE,chart,'--kubeconfig',kubeconfig,
                '--namespace',RELEASE_NAMESPACE,'--create-namespace','--server-side=false']
        deadline.run(helm+['--set','konflux.enabled='+str(enabled).lower()],limit=180)
        wait(['--for=condition=Available','deployment/konflux-operator-controller-manager','-n','konflux-operator'],300)
        if not enabled:
            deadline.run(helm+['--set','konflux.enabled=true'],limit=180)
        wait(['--for=condition=Ready=True','konflux/konflux'],600)
    ready_deadline=min(deadline.end,time.monotonic()+600)
    while True:
        report = observe(kubectl,deadline,uid,source_digest,lock['requiredDeployments'])
        if report['status']=='ready' or time.monotonic()>=ready_deadline:
            break
        time.sleep(min(2,deadline.remaining(2)))
    (state/'konflux-installation-report.json').write_text(json.dumps(report,indent=2)+'\n')
    if report['status']!='ready':
        raise ValueError('Konflux or a deployment is not ready; inspect supply-chain status')
    print(json.dumps({'status':report['status'],'cluster':args.cluster,'release':lock['release'],
                      'deploymentsAvailable':len(report['deployments']),
                      'report':str(state/'konflux-installation-report.json'),
                      'liveWorkflowAdvanced':False,'provenanceValidated':False},indent=2))
    return 0
