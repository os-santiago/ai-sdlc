"""Install bounded build infrastructure; never activate or advance delivery."""
import argparse
from contextlib import ExitStack
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import subprocess

import build_foundation_node
import build_foundation_state
import build_runtime_install
import install_config
import konflux_runtime_install as platform

VERSION='ai-sdlc.build-foundation/v1'
NAMESPACE='ai-sdlc-builds'
KINDS={'Namespace':'namespaces','PersistentVolume':'persistentvolumes','PersistentVolumeClaim':'persistentvolumeclaims',
       'NetworkPolicy':'networkpolicies','LimitRange':'limitranges','ServiceAccount':'serviceaccounts',
       'Secret':'secrets','ConfigMap':'configmaps','ValidatingAdmissionPolicy':'validatingadmissionpolicies',
       'ValidatingAdmissionPolicyBinding':'validatingadmissionpolicybindings','MutatingAdmissionPolicy':'mutatingadmissionpolicies',
       'MutatingAdmissionPolicyBinding':'mutatingadmissionpolicybindings'}


def bounded_json(path,limit=262144):
    path=Path(path)
    if path.is_symlink() or not path.is_file():raise ValueError('Regular reviewed configuration file required')
    with path.open('rb') as stream:raw=stream.read(limit+1)
    if len(raw)>limit:raise ValueError('Configuration exceeds its bound')
    return json.loads(raw,object_pairs_hook=install_config._unique)


def main(argv):
    import install
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command',choices=['plan','local','status'])
    parser.add_argument('--cluster',required=True)
    parser.add_argument('--provider',choices=['auto','podman','docker'],default='auto')
    parser.add_argument('--images',required=True,type=Path)
    parser.add_argument('--publisher-image',required=True)
    parser.add_argument('--controller-digest',required=True)
    parser.add_argument('--approved-trust',type=Path)
    args=parser.parse_args(argv)
    if (not re.fullmatch(r'[a-z][a-z0-9-]{0,35}',args.cluster)
            or not re.fullmatch(r'quay\.io/os-santiago/ai-sdlc@sha256:[a-f0-9]{64}',args.publisher_image)
            or not re.fullmatch(r'sha256:[a-f0-9]{64}',args.controller_digest)
            or args.approved_trust is not None and args.command!='local'):
        raise ValueError('Reviewed local build foundation scope required')
    approved_input = bounded_json(args.approved_trust) if args.approved_trust is not None else None
    lock=install.image_lock(args.images)
    if not re.fullmatch(r'[a-f0-9]{40}',lock['image'].get('tag','')):raise ValueError('Exact reviewed image revision required')
    image=lock['image']['repository']+'@'+lock['migrator']['digest']
    blueprint=bounded_json(install.ROOT/'runtime/k8s/deploy/konflux/build-foundation.json')
    if blueprint.get('apiVersion')!='v1' or blueprint.get('kind')!='List' or len(blueprint.get('items',[]))!=7:
        raise ValueError('Reviewed build foundation manifest is invalid')
    desired={'version':VERSION,'cluster':args.cluster,'revision':lock['image']['tag'],
             'migratorImage':image,'publisherImage':args.publisher_image,'controllerDigest':args.controller_digest,
             'manifestSHA256':hashlib.sha256(build_foundation_state.canonical(blueprint)).hexdigest()}
    if args.command=='plan':
        print(json.dumps({'desired':desired,'resources':[build_foundation_state.identity(x) for x in blueprint['items']],
                          'physicalStorage':{'bytes':4<<30,'inodes':32768,'singleUse':True},
                          'requiresSeparateTrustApproval':True,'deliveryActivated':False,'liveWorkflowAdvanced':False},indent=2))
        return 0
    cache=Path(os.environ.get('XDG_CACHE_HOME',Path.home()/'.cache'))/'ai-sdlc/tools'
    binaries=install.tools(cache)
    state_dir=Path(os.environ.get('XDG_STATE_HOME',Path.home()/'.local/state'))/'ai-sdlc'/args.cluster
    if args.approved_trust is not None:
        for output in ('build-trust-candidate.json','build-foundation.json','build-desired.json'):
            target=state_dir/output
            if args.approved_trust.resolve()==target.resolve() or target.exists() and os.path.samefile(args.approved_trust,target):
                raise ValueError('Approved trust must be a separate immutable input, not an installer output')
    if not state_dir.is_dir() or state_dir.is_symlink():raise ValueError('Existing owned Konflux installation required')
    with ExitStack() as stack:
        platform.lock_state(state_dir,stack)
        provider=platform.select_provider(args.provider,state_dir,args.cluster)
        kubectl=[binaries['kubectl'],'--kubeconfig',state_dir/'kubeconfig','--context','kind-'+args.cluster]
        def call(argv,**kwargs):
            return install.run(kubectl+argv,capture_output=True,timeout=45,**kwargs)
        def get(kind,name,namespace=None,optional=False):
            if kind not in KINDS.values() and kind not in ('nodes','leases'):
                raise ValueError('Unsupported foundation inventory read')
            raw=call(['get',kind,name,*(['-n',namespace] if namespace else []),*(['--ignore-not-found'] if optional else []),'-o','json']).stdout
            return json.loads(raw) if raw.strip() else None
        cluster_uid=get('namespaces','kube-system')['metadata']['uid']
        platform.verify_cluster(install.run,kubectl,state_dir,args.cluster,provider,cluster_uid)
        # Every administrative mutating operation requires frozen runtime and
        # must not remove or change an enabled controller's cleanup authority.
        if args.command=='local':platform.require_frozen(install.run,kubectl)
        name=args.cluster+'-control-plane'
        node=json.loads(install.run([provider,'inspect',name],capture_output=True,timeout=15).stdout)[0]
        if node.get('Config',{}).get('Labels',{}).get('io.x-k8s.kind.cluster')!=args.cluster or not re.fullmatch(r'[a-f0-9]{64}',node.get('Id','')):
            raise ValueError('Original owned node container required')
        node_obj=get('nodes',name)
        scope=dict(desired,provider=provider,clusterUID=cluster_uid,nodeID=node['Id'],nodeUID=node_obj['metadata']['uid'],nodeBootID=node_obj['status']['nodeInfo']['bootID'])
        path=state_dir/'build-foundation.json'
        if path.exists() or path.is_symlink():
            state=bounded_json(path)
            if any(state.get(key)!=value for key,value in scope.items()) or not isinstance(state.get('resources'),dict):
                raise ValueError('Build foundation installation identity or desired state changed')
        else:
            if args.command!='local':raise ValueError('Build foundation is not installed')
            state=dict(scope,storageIdentitySHA256=secrets.token_hex(32),resources={})
            platform.write_record(path,state)
        def save():platform.write_record(path,state)
        def fence():
            if state.get('namespaceUID') is not None and get('namespaces',NAMESPACE)['metadata']['uid']!=state['namespaceUID']:
                raise ValueError('Original build namespace identity changed')
            if get('namespaces','kube-system')['metadata']['uid']!=state['clusterUID']:
                raise ValueError('Original cluster identity changed')
            current=get('nodes',name)
            if current['metadata']['uid']!=state['nodeUID'] or current['status']['nodeInfo']['bootID']!=state['nodeBootID']:
                raise ValueError('Original node or boot identity changed')
            current_node=json.loads(install.run([provider,'inspect',name],capture_output=True,timeout=15).stdout)[0]
            if current_node.get('Id')!=state['nodeID']:raise ValueError('Original kind node container replaced')
        def read(obj):
            return get(KINDS[obj['kind']],obj['metadata']['name'],obj['metadata'].get('namespace'),True)
        def create(obj):
            fence()
            if args.command!='local':raise ValueError('Read-only foundation status cannot create resources')
            return json.loads(call(['create','-f','-','-o','json'],input=json.dumps(obj)).stdout)
        resources=build_foundation_state.Resources(path,state,read,create)
        namespace={'apiVersion':'v1','kind':'Namespace','metadata':{'name':NAMESPACE,'labels':{'pod-security.kubernetes.io/enforce':'restricted'}}}
        if args.command=='status':
            for key,entry in state['resources'].items():
                if entry.get('uid') is None:raise ValueError('Build foundation has unresolved create intent')
                api,kind,namespace_name,object_name=key.rsplit('/',3)
                current=get(KINDS[kind],object_name,namespace_name or None)
                if (current['apiVersion']!=api or current['kind']!=kind or current['metadata'].get('uid')!=entry['uid']
                        or current['metadata'].get('deletionTimestamp') is not None
                        or current['metadata'].get('annotations',{}).get(build_foundation_state.ANNOTATION)!=entry['nonce']
                        or hashlib.sha256(build_foundation_state.canonical(build_foundation_state.body(current))).hexdigest()!=entry['desiredSHA256']):
                    raise ValueError('Original foundation resource or desired state differs')
            fence()
            print(json.dumps({'recordedStatus':state.get('status','incomplete'),'cluster':args.cluster,'namespaceUID':state['namespaceUID'],
                              'resourcesChecked':len(state['resources']),'fullProfileReadinessValidated':False,
                              'deliveryActivated':False,'liveWorkflowAdvanced':False},indent=2))
            return 0
        ns=resources.ensure(namespace)
        if state.get('namespaceUID') not in (None,ns['metadata']['uid']):raise ValueError('Original foundation namespace changed')
        state['namespaceUID']=ns['metadata']['uid'];save()
        fence()
        build_foundation_node.initialize(install.run,provider,state['nodeID'],image,state,save)
        pv_name='ai-sdlc-build-'+state['storageIdentitySHA256'][:32]
        pvc=resources.ensure({'apiVersion':'v1','kind':'PersistentVolumeClaim','metadata':{'name':'build-slot-0','namespace':NAMESPACE},
                             'spec':{'storageClassName':'','volumeName':pv_name,'accessModes':['ReadWriteOnce'],'resources':{'requests':{'storage':'4Gi'}}}})
        pv=resources.ensure({'apiVersion':'v1','kind':'PersistentVolume','metadata':{'name':pv_name},'spec':{
            'capacity':{'storage':'4Gi'},'accessModes':['ReadWriteOnce'],'persistentVolumeReclaimPolicy':'Retain','storageClassName':'',
            'local':{'path':state['mountIntent']['target']},'claimRef':{'apiVersion':'v1','kind':'PersistentVolumeClaim','namespace':NAMESPACE,'name':'build-slot-0','uid':pvc['metadata']['uid']},
            'nodeAffinity':{'required':{'nodeSelectorTerms':[{'matchExpressions':[{'key':'kubernetes.io/hostname','operator':'In','values':[name]}]}]}}}})
        call(['wait','-n',NAMESPACE,'--for=jsonpath={.status.phase}=Bound','pvc/build-slot-0','--timeout=30s'])
        slot={'clusterUID':state['clusterUID'],'namespaceUID':state['namespaceUID'],'pvcUID':pvc['metadata']['uid'],'pvUID':pv['metadata']['uid'],
              'nodeUID':state['nodeUID'],'nodeBootID':state['nodeBootID'],'storageIdentitySHA256':state['storageIdentitySHA256'],
              'namespace':NAMESPACE,'pvc':'build-slot-0','node':name,'capacityBytes':4<<30,'inodeLimit':32768}
        for item in blueprint['items']:resources.ensure(item)
        secret=get('secrets','internal-registry-client-auth','kind-registry')
        if secret.get('type')!='kubernetes.io/dockerconfigjson' or set(secret.get('data',{}))!={'.dockerconfigjson'}:
            raise ValueError('Expected installed Konflux registry credential')
        origin={'uid':secret['metadata']['uid'],'dataSHA256':hashlib.sha256(build_foundation_state.canonical(secret['data'])).hexdigest()}
        if state.get('registryCredentialSource',origin)!=origin:raise ValueError('Registry credential source changed')
        state['registryCredentialSource']=origin;save()
        resources.ensure({'apiVersion':'v1','kind':'Secret','metadata':{'name':'registry-auth','namespace':NAMESPACE},'immutable':True,'type':secret['type'],'data':secret['data']})
        cfg=json.loads(call(['config','view','--raw','--minify','-o','json']).stdout)
        cluster_config,user=cfg['clusters'][0]['cluster'],cfg['users'][0]['user']
        client={'APIServer':'https://127.0.0.1:6443','CA':cluster_config['certificate-authority-data'],
                'ClientCertificate':user['client-certificate-data'],'ClientKey':user['client-key-data']}
        def observe(operation,value):
            fence()
            return build_runtime_install.component_call(provider,image,operation,value,state['nodeID'])
        admission=observe('observe-build-admission',{'client':client,'slots':[slot],'publisherImage':args.publisher_image})
        policies=observe('build-admission',{'admission':admission,'slots':[slot]})
        if policies.get('kind')!='List' or len(policies.get('items',[]))!=4:raise ValueError('Invalid finite admission definitions')
        for item in policies['items']:resources.ensure(item)
        trust=observe('observe-build-trust',{'client':client,'slots':[slot]})
        candidate=state_dir/'build-trust-candidate.json'
        platform.write_record(candidate,trust)
        state['status']='awaiting-trust-review';state['slot']=slot;save()
        if args.approved_trust is None:
            print(json.dumps({'status':state['status'],'candidate':str(candidate),'deliveryActivated':False,'liveWorkflowAdvanced':False},indent=2))
            return 0
        approved=approved_input
        if approved!=trust:raise ValueError('Separately approved platform trust differs from current observation')
        resources.ensure({'apiVersion':'v1','kind':'ConfigMap','metadata':{'name':'build-trust','namespace':NAMESPACE},'immutable':True,'data':{'trust.json':json.dumps(approved,sort_keys=True)}})
        profile=observe('observe-build-profile',{'client':client,'slots':[slot],'admission':admission,'approvedTrust':approved})
        bundle={'version':'ai-sdlc.build-desired/v1','revision':lock['image']['tag'],'controllerDigest':args.controller_digest,'profile':profile,'slots':[slot],'issueMaxAgeSeconds':60}
        bundle_path=state_dir/'build-desired.json';platform.write_record(bundle_path,bundle)
        build_runtime_install.load_bundle(bundle_path,lock)
        state['status']='prepared';state['bundleSHA256']=hashlib.sha256(build_foundation_state.canonical(bundle)).hexdigest();save()
        print(json.dumps({'status':state['status'],'bundle':str(bundle_path),'originalNamespaceUID':state['namespaceUID'],
                          'deliveryActivated':False,'liveWorkflowAdvanced':False,'releaseAuthorized':False},indent=2))
        return 0
