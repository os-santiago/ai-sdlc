"""Install public registry resolution and TLS trust on an original frozen kind node."""
import argparse
from contextlib import ExitStack
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import ssl
import tomllib

import build_foundation_install
import konflux_runtime_install as platform
import managed_registry_install as registry
import registry_node_files as files
import supply_chain_install

VERSION='ai-sdlc.registry-node-trust/v1'


def registry_path(config,version):
    # This is the effective v4 dump from the reviewed kind containerd version.
    if version!='2.3.4':raise ValueError('Unreviewed node containerd version')
    value=tomllib.loads(config)
    settings=value.get('plugins',{}).get('io.containerd.cri.v1.images',{}).get('registry',{})
    path=settings.get('config_path')
    if (value.get('version')!=4 or not isinstance(path,str) or settings.get('mirrors')
            or settings.get('configs') or settings.get('auths')
            or path not in ('','/etc/containerd/certs.d','/etc/containerd/certs.d:/etc/docker/certs.d')):
        raise ValueError('Node registry configuration is outside the reviewed scope')
    # v2.3.4 InitFn defaults the empty path on Linux without mirrors. A dump
    # alone does not show that initialization; actual pull is a separate gate.
    return '/etc/containerd/certs.d'


def main(argv):
    import install
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command',choices=('plan','local','status'))
    parser.add_argument('--cluster',required=True)
    parser.add_argument('--provider',choices=('podman','docker'),default='podman')
    parser.add_argument('--registry-ca',required=True,type=Path,help='Separately reviewed public registry CA; never a key')
    args=parser.parse_args(argv)
    if not re.fullmatch(r'[a-z][a-z0-9-]{0,35}',args.cluster):raise ValueError('Original local cluster required')
    if args.registry_ca.is_symlink() or not args.registry_ca.is_file():raise ValueError('Regular reviewed public CA required')
    with args.registry_ca.open() as stream:ca=stream.read(65537)
    if not 0<len(ca)<=65536 or 'PRIVATE KEY' in ca:raise ValueError('Bounded public CA required')
    ssl.create_default_context(cadata=ca)
    state_dir=Path(os.environ.get('XDG_STATE_HOME',Path.home()/'.local/state'))/'ai-sdlc'/args.cluster
    if not state_dir.is_dir() or state_dir.is_symlink():raise ValueError('Original installation state required')
    binaries=install.tools(Path(os.environ.get('XDG_CACHE_HOME',Path.home()/'.cache'))/'ai-sdlc/tools')
    deadline=supply_chain_install.Deadline(600)
    def run(argv,**kwargs):return deadline.run(argv,limit=kwargs.pop('timeout',25),**kwargs)
    kubectl=[str(binaries['kubectl']),'--kubeconfig',str(state_dir/'kubeconfig'),'--context','kind-'+args.cluster]
    def get(kind,name,namespace=None):
        raw=run(kubectl+['get',kind,name,*(['-n',namespace] if namespace else []),'-o','json'],capture_output=True,timeout=20).stdout
        if len(raw)>1048576:raise ValueError('Node trust inventory exceeds bound')
        return json.loads(raw)
    with ExitStack() as stack:
        platform.lock_state(state_dir,stack)
        cluster_uid=get('namespace','kube-system')['metadata']['uid']
        platform.verify_cluster(run,kubectl,state_dir,args.cluster,args.provider,cluster_uid)
        original=registry.read_record(state_dir/'managed-registry.json')
        if (original.get('clusterUID')!=cluster_uid or original.get('replacementProof',{}).get('status')!='passed'
                or original.get('replacementProof',{}).get('workflowEvidence') is not False):
            raise ValueError('Original independently accepted managed registry required')
        observer=registry.Installer(state_dir,original,kubectl,binaries,deadline,cluster_uid,read_only=True)
        def registry_fence():
            observer.fence(cold=False);observer.check_chart_objects()
            if observer.storage()!=original['storage']:raise ValueError('Original registry storage changed')
        registry_fence()
        service=get('service','registry-service','kind-registry')
        address=service['spec']['clusterIP']
        ip=ipaddress.ip_address(address)
        if ip.version!=4 or not ip.is_private or ip.is_loopback or ip.is_link_local:raise ValueError('Reviewed private registry IPv4 required')
        node_name=args.cluster+'-control-plane'
        node=get('node',node_name)
        container=json.loads(run([args.provider,'inspect',node_name],capture_output=True,timeout=15).stdout)[0]
        node_id=container.get('Id','')
        if not re.fullmatch('[a-f0-9]{64}',node_id) or container.get('Config',{}).get('Labels',{}).get('io.x-k8s.kind.cluster')!=args.cluster:
            raise ValueError('Original kind node container required')
        def node_call(command,**kwargs):
            result=run([args.provider,'exec',*(['-i'] if 'input' in kwargs else []),node_id,*command],
                capture_output=True,timeout=25,check=False,**kwargs)
            if result.returncode or len(result.stdout)>1048576:
                raise ValueError('Bounded node trust operation rejected')
            return result
        config=node_call(['containerd','config','dump']).stdout
        version=node['status']['nodeInfo']['containerRuntimeVersion'].removeprefix('containerd://')
        registry_path(config,version)
        desired={'version':VERSION,'clusterUID':cluster_uid,'cluster':args.cluster,'provider':args.provider,
            'nodeID':node_id,'nodeUID':node['metadata']['uid'],'nodeBootID':node['status']['nodeInfo']['bootID'],
            'serviceUID':service['metadata']['uid'],'address':address,'caSHA256':files.sha(ca),
            'containerdVersion':version,'containerdConfigSHA256':files.sha(config),
            'registryObjectsSHA256':registry.digest(original['chartObjects']),'storage':original['storage']}
        foundation_path=state_dir/'build-foundation.json'
        if foundation_path.exists() or foundation_path.is_symlink():
            foundation=build_foundation_install.bounded_json(foundation_path)
            if any(foundation.get(key)!=desired[key] for key in ('clusterUID','nodeID','nodeUID','nodeBootID')):
                raise ValueError('Original build foundation node custody differs')
        path=state_dir/'registry-node-trust.json'
        state=build_foundation_install.bounded_json(path) if path.exists() or path.is_symlink() else None
        if state is not None and any(state.get(key)!=value for key,value in desired.items()):
            raise ValueError('Original registry node trust scope changed')
        if state is not None and not re.fullmatch('[a-f0-9]{48}',str(state.get('nonce',''))):
            raise ValueError('Original registry node nonce is invalid')
        if args.command=='plan':
            print(json.dumps({'desired':desired,'files':['/etc/hosts',files.ROOT+'/hosts.toml',files.ROOT+'/ca.crt'],
                'containerdRestartRequired':False,'imagePullValidated':False,'workflowEvidence':False},indent=2));return 0
        mutate=args.command=='local'
        if mutate:platform.require_frozen(run,kubectl)
        if state is None:
            if not mutate:raise ValueError('Node trust is not installed')
            state=dict(desired,nonce=secrets.token_hex(24),files={});platform.write_record(path,state)
        def save():platform.write_record(path,state)
        def fence():
            current=get('node',node_name)
            if current['metadata']['uid']!=desired['nodeUID'] or current['status']['nodeInfo']['bootID']!=desired['nodeBootID']:
                raise ValueError('Original node or boot identity changed')
            actual=json.loads(run([args.provider,'inspect',node_name],capture_output=True,timeout=15).stdout)[0]
            if actual.get('Id')!=node_id:raise ValueError('Original node container replaced')
            endpoint=get('service','registry-service','kind-registry')
            if endpoint['metadata']['uid']!=desired['serviceUID'] or endpoint['spec']['clusterIP']!=address:
                raise ValueError('Original registry service changed')
            if files.sha(node_call(['containerd','config','dump']).stdout)!=desired['containerdConfigSHA256']:
                raise ValueError('Node registry configuration changed')
            if mutate:platform.require_frozen(run,kubectl)
        def guarded(command,**kwargs):fence();return node_call(command,**kwargs)
        journal=files.Files(guarded,state,save)
        journal.directory(mutate)
        journal.ensure('ca.crt',ca,mutate)
        hosts='server = "https://'+files.HOST+'"\n[host."https://'+files.HOST+'"]\n  capabilities = ["pull", "resolve"]\n  ca = "'+files.ROOT+'/ca.crt"\n'
        journal.ensure('hosts.toml',hosts,mutate)
        journal.hosts(address,mutate)
        # Ordinary node DNS, CA and hostname validation; no --resolve, proxy,
        # alternate host, skip-verify, password or runtime stage invocation.
        code=node_call(['curl','--noproxy','*','--silent','--show-error','--max-time','10','--connect-timeout','3',
            '--proto','=https','--tlsv1.3','--cacert',files.ROOT+'/ca.crt','--output','/dev/null','--write-out','%{http_code}',
            'https://'+files.HOST+'/v2/ai-sdlc-fixtures/saferus/manifests/'+original['conformance']['markerDigest']]).stdout
        if code!='401':raise ValueError('Node TLS or anonymous registry denial was not demonstrated')
        fence();registry_fence()
        report={'version':VERSION,'status':'configured','nodeUID':desired['nodeUID'],'tlsHostnameVerified':files.HOST,
            'tlsMinimum':'TLS1.3','anonymousReadDenied':True,'imagePullValidated':False,'workloadExecutionValidated':False,
            'containerdRestarted':False,'workflowEvidence':False,'deliveryActivated':False}
        if mutate:state['report']=report;save()
        print(json.dumps(report,indent=2));return 0
