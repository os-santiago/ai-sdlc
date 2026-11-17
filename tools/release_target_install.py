"""Prepare original Saferus registry-reader custody; never activate delivery."""
import argparse
import base64
import copy
from contextlib import ExitStack
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile

import bootstrap_prepare
import build_foundation_install as files
import build_foundation_state
import build_runtime_install
import konflux_runtime_install as platform
import prepared_configuration
import managed_registry_install
import supply_chain_install
import release_target_oci
import release_target_resources

VERSION='ai-sdlc.release-target-preparation/v1'


def private_directory(path):
    value=path.lstat()
    if path.is_symlink() or not stat.S_ISDIR(value.st_mode) or value.st_uid!=os.getuid() or value.st_mode&0o077:
        raise ValueError('Original owned private state directory required')


class Preparation:
    def __init__(self,run,kubectl,provider,binaries,state,saferus,cluster,lock,configuration):
        self.run,self.kubectl,self.provider,self.binaries=run,kubectl,provider,binaries
        self.state,self.saferus,self.cluster,self.lock,self.configuration=state,saferus,cluster,lock,configuration
        self.path=state/'release-target-preparation.json'
        self.record=files.bounded_json(self.path,2*1024*1024) if self.path.exists() or self.path.is_symlink() else None
        if self.record is not None and not isinstance(self.record,dict):raise ValueError('Malformed original target preparation record')
        self.owner=files.bounded_json(saferus/'environment-ownership.json')
        self.intent=files.bounded_json(saferus/'environment-intent.json')
        self.resources=files.bounded_json(saferus/'console-resources.json')
        self.values=files.bounded_json(saferus/'values.json')
        if not all(isinstance(x,dict) for x in (self.owner,self.intent,self.resources,self.values)) or not isinstance(self.values.get('console'),dict):raise ValueError('Malformed original application custody')
        ca_path=saferus/'ca.crt'
        fd=os.open(ca_path,os.O_RDONLY|os.O_NOFOLLOW)
        try:
            info=os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_uid!=os.getuid() or info.st_size>65536:raise ValueError('Original bounded CA file required')
            with os.fdopen(fd,'rb',closefd=False) as stream:self.ca=stream.read(65537).decode()
        finally:os.close(fd)
        if (set(self.owner)!={'cluster','clusterUID','namespace','namespaceUID'} or self.owner['cluster']!=cluster
                or self.owner['namespace']!='ai-sdlc-production' or set(self.intent)!={'clusterUID','namespace','nonce'}
                or self.intent['clusterUID']!=self.owner['clusterUID'] or self.intent['namespace']!=self.owner['namespace']
                or not re.fullmatch('[a-f0-9]{48}',self.intent['nonce']) or set(self.resources)!={'Deployment','Service','Secret','NetworkPolicy'}
                or not 1<=len(self.ca)<=65536):raise ValueError('Original Saferus installation custody required')
        self.source={'owner':self.owner,'intentSHA256':bootstrap_prepare.digest(self.intent),'resourceUIDs':self.resources,
            'baselineImage':self.values.get('console',{}).get('image'),'caSHA256':hashlib.sha256(self.ca.encode()).hexdigest()}
        if self.record is not None and (self.record.get('version')!=VERSION or self.record.get('source')!=self.source):
            raise ValueError('Original release target preparation source changed')

    def guard(self,inspect=False):
        if self.configuration.record is None or self.configuration.record.get('phase')!='configured-paused':
            raise ValueError('Complete configured-paused runtime required')
        observed=self.configuration.guard(inspect=inspect)
        if observed['clusterUID']!=self.owner['clusterUID']:raise ValueError('Original runtime cluster changed')
        self.namespace()
        return observed

    def namespace(self):
        namespace=self.get('namespace','ai-sdlc-production',None)
        meta=(namespace or {}).get('metadata',{})
        if (namespace is None or bootstrap_prepare.Preparation.identity(namespace)!=self.owner['namespaceUID']
                or meta.get('ownerReferences') or meta.get('annotations',{}).get('saferus.io/creation-nonce')!=self.intent['nonce']
                or meta.get('annotations',{}).get('saferus.io/cluster-uid')!=self.owner['clusterUID']):
            raise ValueError('Original application namespace changed')
        return namespace

    def get(self,kind,name,namespace='ai-sdlc-production'):
        raw=self.run(self.kubectl+['get',kind,name,*(['-n',namespace] if namespace else []),'--ignore-not-found','-o','json'],capture_output=True,timeout=20).stdout
        if len(raw)>1024*1024:raise ValueError('Target observation exceeded bound')
        return json.loads(raw) if raw.strip() else None

    def original(self,kind,name):
        value=self.get(kind,name)
        if value is None or bootstrap_prepare.Preparation.identity(value)!=self.resources[kind]:raise ValueError('Original application resource replaced')
        meta=value['metadata']
        if meta.get('ownerReferences') or meta.get('finalizers') or meta.get('annotations',{}).get('saferus.io/creation-nonce')!=self.intent['nonce']:
            raise ValueError('Original application resource custody changed')
        return value

    def source_credentials(self):
        self.guard()
        registry=managed_registry_install.read_record(self.state/'managed-registry.json')
        if registry.get('clusterUID')!=self.owner['clusterUID']:raise ValueError('Original registry cluster changed')
        observer=managed_registry_install.Installer(self.state,registry,self.kubectl,self.binaries,supply_chain_install.Deadline(120),self.owner['clusterUID'],read_only=True)
        observer.fence(cold=False);observer.check_chart_objects()
        source=self.get('secret','ai-sdlc-provenance-registry','ai-sdlc-runtime')
        if source is None or source.get('immutable') is not True or source.get('type')!='Opaque' or set(source.get('data',{}))!={'credential.json'}:
            raise ValueError('Original installed provenance reader required')
        credential=json.loads(base64.b64decode(source['data']['credential.json'],validate=True))
        if (set(credential)!={'username','password'} or not re.fullmatch('[A-Za-z0-9_-]{1,64}',credential['username'])
                or not re.fullmatch('[A-Za-z0-9_-]{32,128}',credential['password'])):raise ValueError('Malformed original reader custody')
        auth=base64.b64encode((credential['username']+':'+credential['password']).encode()).decode()
        config={'auths':{'registry-service.kind-registry':{'auth':auth}}}
        desired={'apiVersion':'v1','kind':'Secret','metadata':{'name':'ai-sdlc-release-reader','namespace':'ai-sdlc-production',
            'annotations':{'ai-sdlc.io/cluster-uid':self.owner['clusterUID'],'ai-sdlc.io/reader-source-uid':source['metadata']['uid']}},
            'type':'kubernetes.io/dockerconfigjson','immutable':True,'data':{'.dockerconfigjson':base64.b64encode(json.dumps(config,sort_keys=True).encode()).decode()}}
        return source['metadata']['uid'],desired

    def baseline_binding(self,deployment):
        pod_list=json.loads(self.run(self.kubectl+['get','pods','-n','ai-sdlc-production','-l','app=saferus-console','-o','json'],capture_output=True,timeout=20).stdout)
        if len(pod_list.get('items',[]))!=1:raise ValueError('One original ready baseline Pod required')
        pod=pod_list['items'][0];owners=pod['metadata'].get('ownerReferences',[])
        if len(owners)!=1 or owners[0].get('kind')!='ReplicaSet' or owners[0].get('controller') is not True:raise ValueError('Baseline Pod owner unavailable')
        rs=self.get('replicaset',owners[0]['name']);rs_owners=(rs or {}).get('metadata',{}).get('ownerReferences',[])
        if (rs is None or rs['metadata']['uid']!=owners[0]['uid'] or len(rs_owners)!=1
                or rs_owners[0].get('uid')!=deployment['metadata']['uid'] or rs_owners[0].get('controller') is not True
                or pod['metadata'].get('deletionTimestamp') or pod['status'].get('phase')!='Running'
                or not any(c.get('type')=='Ready' and c.get('status')=='True' for c in pod['status'].get('conditions',[]))):
            raise ValueError('Original baseline ownership or readiness differs')
        containers=pod['spec'].get('containers',[]);statuses=pod['status'].get('containerStatuses',[])
        if len(containers)!=1 or len(statuses)!=1 or containers[0].get('image')!=self.source['baselineImage'] or statuses[0].get('name')!='console' or statuses[0].get('ready') is not True:
            raise ValueError('Original baseline image changed')
        reference=self.source['baselineImage'].split('@')[-1];runtime=statuses[0].get('imageID','').split('@')[-1]
        observation=self.guard();node_id=observation['nodeID']
        def read(digest,limit):
            # An owned node and hash-selected blob; no shell or image importer.
            return release_target_oci.bounded_output([self.provider,'exec',node_id,'ctr','-n','k8s.io','content','get',digest],limit)
        return release_target_oci.verify(reference,runtime,read)

    def describe(self,deployment,service):
        image=self.lock['image']['repository']+'@'+self.lock['migrator']['digest']
        specs=build_runtime_install.component_call(self.provider,image,'describe-release-specs',{'deployment':deployment,'service':service})
        if (set(specs)!={'version','deploymentSpecSHA256','serviceSpecSHA256','baselineImage'} or specs['version']!='ai-sdlc.release-specs/v1'
                or specs['baselineImage']!=self.source['baselineImage'] or any(not re.fullmatch('[a-f0-9]{64}',specs[field]) for field in ('deploymentSpecSHA256','serviceSpecSHA256'))):
            raise ValueError('Public target description differs')
        return specs

    def prepare(self,read_only=False):
        self.guard(inspect=True)
        if read_only and (self.record is None or not self.record.get('preparedTargetSHA256') or not (self.record.get('deploymentPatch') or {}).get('response')):
            raise ValueError('Status requires completed target preparation')
        current=self.original('Deployment','saferus-console');service=self.original('Service','saferus-console')
        tls=self.original('Secret','saferus-console-tls');self.original('NetworkPolicy','saferus-console')
        if current['spec']['template']['spec']['containers'][0]['image']!=self.source['baselineImage']:raise ValueError('Original baseline image changed')
        ca=self.get('configmap','saferus-ca')
        if ca is None or ca.get('data',{}).get('ca.crt')!=self.ca or tls.get('type')!='kubernetes.io/tls':raise ValueError('Original application CA/TLS binding changed')
        self.describe(current,service) # pure compiler preflight before all effects
        reader_uid,secret=self.source_credentials()
        binding=self.baseline_binding(current)
        if self.record is None:
            if read_only:raise ValueError('Target preparation has not been performed')
            release_target_resources.patch_request(current,'0'*48) # preflight before intent
            self.record={'version':VERSION,'source':self.source,'readerSourceUID':reader_uid,'secret':secret,'baselineBinding':binding,'resources':{},'deploymentPatch':None}
            platform.write_record(self.path,self.record)
        elif self.record['readerSourceUID']!=reader_uid or self.record['secret']!=secret or self.record['baselineBinding']!=binding:
            raise ValueError('Original target reader or OCI custody changed')
        def get(obj):return self.get(obj['kind'],obj['metadata']['name'])
        def create(obj):
            if read_only:raise ValueError('Status cannot create a credential')
            self.guard()
            return json.loads(self.run(self.kubectl+['create','-f','-','-o','json'],input=json.dumps(obj),capture_output=True,timeout=20).stdout)
        if read_only:
            entry=self.record['resources'].get(build_foundation_state.identity(secret),{})
            if not entry.get('uid'):raise ValueError('Status cannot resolve an incomplete credential creation')
        pull=build_foundation_state.Resources(self.path,self.record,get,create).ensure(secret)
        before=self.record['deploymentPatch']['before'] if self.record.get('deploymentPatch') else current
        def send(patch):
            if read_only:raise ValueError('Status cannot configure a Deployment')
            self.guard()
            # stdin avoids recording the conditional patch in process arguments.
            return json.loads(self.run(self.kubectl+['patch','deployment','saferus-console','-n','ai-sdlc-production','--type=json','--patch-file=/dev/stdin','-o','json'],input=json.dumps(patch),capture_output=True,timeout=20).stdout)
        if read_only:
            patched=current;release_target_resources.verify_after(before,patched,self.record['deploymentPatch']['nonce'])
        else:
            patched=release_target_resources.Patch(self.path,self.record,self.guard,lambda:self.original('Deployment','saferus-console'),send).apply(before)
        self.run(self.kubectl+['rollout','status','deployment/saferus-console','-n','ai-sdlc-production','--timeout=180s'],capture_output=True,timeout=190)
        patched=self.original('Deployment','saferus-console');release_target_resources.verify_after(before,patched,self.record['deploymentPatch']['nonce'])
        if self.baseline_binding(patched)!=binding:raise ValueError('Prepared baseline runtime content changed')
        final_service=self.original('Service','saferus-console');final_tls=self.original('Secret','saferus-console-tls');final_ca=self.get('configmap','saferus-ca')
        if final_service!=service or final_tls!=tls or final_ca!=ca:raise ValueError('Original target service or TLS material changed during preparation')
        specs=self.describe(patched,service)
        target={'version':'ai-sdlc.saferus-release-target/v1','scope':'kind-test-production','repository':'os-santiago/saferus','environment':'ai-sdlc-production',
            'clusterUID':self.owner['clusterUID'],'namespaceUID':self.owner['namespaceUID'],'namespace':'ai-sdlc-production','deployment':'saferus-console','deploymentUID':patched['metadata']['uid'],
            'container':'console','deploymentSpecSHA256':specs['deploymentSpecSHA256'],'service':'saferus-console','serviceUID':service['metadata']['uid'],'serviceSpecSHA256':specs['serviceSpecSHA256'],
            'pullSecret':'ai-sdlc-release-reader','pullSecretUID':pull['metadata']['uid'],'tlsSecretUID':tls['metadata']['uid'],'tlsCA':self.ca,'tlsHostname':'saferus-console',
            'baselineImage':self.source['baselineImage'],'baselineRuntimeDigest':binding['runtimeDigest'],'rolloutSeconds':300,'smokeSeconds':30,'approvedAt':0,'expiresAt':0}
        self.guard(inspect=True)
        digest=bootstrap_prepare.digest(target)
        if self.record.get('preparedTargetSHA256') is not None and self.record['preparedTargetSHA256']!=digest:
            raise ValueError('Completed target preparation cannot silently replace its candidate')
        if read_only:
            if self.record['preparedTargetSHA256']!=digest or files.bounded_json(self.state/'release-target-candidate.json')!=target:raise ValueError('Original target candidate changed')
        else:
            platform.write_record(self.state/'release-target-candidate.json',target)
            self.record['preparedTargetSHA256']=digest;platform.write_record(self.path,self.record)
        return {'version':VERSION,'configuration':'prepared-unapproved','workflowAdvanced':False,'imageChanged':False,'targetSHA256':self.record['preparedTargetSHA256'],
            'candidate':str(self.state/'release-target-candidate.json'),'baselineBinding':binding}


def main(argv):
    import install
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command',choices=('prepare','status'));parser.add_argument('--cluster',required=True)
    parser.add_argument('--provider',choices=('auto','podman','docker'),default='auto');parser.add_argument('--images',required=True,type=Path)
    parser.add_argument('--saferus-state',required=True,type=Path);args=parser.parse_args(argv)
    if not re.fullmatch('[a-z][a-z0-9-]{0,35}',args.cluster):raise ValueError('Selected local cluster required')
    lock=install.image_lock(args.images)
    if not re.fullmatch('[a-f0-9]{40}',lock['image'].get('tag','')):raise ValueError('Exact reviewed compiler revision required')
    state=Path(os.environ.get('XDG_STATE_HOME',Path.home()/'.local/state'))/'ai-sdlc'/args.cluster
    private_directory(state);private_directory(args.saferus_state)
    if state.resolve()==args.saferus_state.resolve():raise ValueError('Application custody must remain separate')
    with ExitStack() as stack:
        platform.lock_state(state,stack)
        fd=os.open(args.saferus_state/'lock',os.O_CREAT|os.O_RDWR|os.O_NOFOLLOW,0o600);stack.callback(os.close,fd);fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB)
        provider=platform.select_provider(args.provider,state,args.cluster)
        binaries=install.tools(Path(os.environ.get('XDG_CACHE_HOME',Path.home()/'.cache'))/'ai-sdlc/tools')
        kubectl=[binaries['kubectl'],'--kubeconfig',str(state/'kubeconfig'),'--context','kind-'+args.cluster,'--request-timeout=10s']
        config=prepared_configuration.Configuration(install.run,kubectl,binaries,provider,args.cluster,state,{})
        result=Preparation(install.run,kubectl,provider,binaries,state,args.saferus_state,args.cluster,lock,config).prepare(args.command=='status')
        print(json.dumps(result,indent=2));return 0
