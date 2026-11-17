"""Probe the installed paused release network with disposable, tokenless Pods.

Administrative connectivity evidence only: no workflow input, image patch,
cryptographic demo POST, release or database stage write is performed.
"""
import argparse
from contextlib import ExitStack
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import time

import install
import bootstrap_prepare
import build_runtime_install
import konflux_runtime_install as platform
import prepared_configuration
from release_target_install import private_directory

# Denial must come from a live client's bounded network attempt. TLS validation,
# malformed replies, image/exec/API failures are never counted as isolation.
CLIENT = r"""
const o=JSON.parse(process.argv[1]);let done=false;
const end=(c,s)=>{if(!done){done=true;console.log(s);process.exit(c)}};
const timer=setTimeout(()=>end(3,'probe:denied'),3000);
const m=require(o.tls?'https':'http');
const q=m.get({host:o.host,port:o.port,path:o.tls?'/health/live':'/',
 ca:o.ca,servername:'saferus-console',rejectUnauthorized:true,agent:false,
 timeout:1500},r=>{let body='';r.on('data',b=>{body+=b;if(body.length>1024)end(2,'probe:invalid')});
 r.on('end',()=>end(r.statusCode===(o.tls?204:200)&&body===(o.tls?'':'network-probe')?0:2,'probe:allowed'))});
q.on('timeout',()=>{q.destroy();end(3,'probe:denied')});
q.on('error',e=>end(['ETIMEDOUT','ECONNREFUSED','EHOSTUNREACH','ENETUNREACH'].includes(e.code)?3:2,'probe:'+(['ETIMEDOUT','ECONNREFUSED','EHOSTUNREACH','ENETUNREACH'].includes(e.code)?'denied':'invalid')));
"""


def outcome(result):
    pair=(result.returncode,result.stdout.strip())
    if pair==(0,'probe:allowed'):return True
    if pair==(3,'probe:denied'):return False
    raise ValueError('Probe did not produce an authenticated connectivity result')


def pod(namespace,name,image,labels,nonce,server=False):
    code="require('http').createServer((q,s)=>s.end('network-probe')).listen(8080,'0.0.0.0')" if server else 'setInterval(()=>{},1000)'
    container={'name':'probe','image':image,'imagePullPolicy':'IfNotPresent','command':['node','-e',code],
        'resources':{'requests':{'cpu':'10m','memory':'32Mi'},'limits':{'cpu':'100m','memory':'128Mi','ephemeral-storage':'16Mi'}},
        'securityContext':{'runAsNonRoot':True,'runAsUser':1000,'readOnlyRootFilesystem':True,'allowPrivilegeEscalation':False,'capabilities':{'drop':['ALL']}}}
    if server:container['readinessProbe']={'tcpSocket':{'port':8080},'periodSeconds':1}
    return {'apiVersion':'v1','kind':'Pod','metadata':{'name':name,'namespace':namespace,'labels':labels,
        'annotations':{'ai-sdlc.io/network-probe-nonce':nonce}},'spec':{'automountServiceAccountToken':False,
        'restartPolicy':'Never','activeDeadlineSeconds':300,'terminationGracePeriodSeconds':1,
        'securityContext':{'seccompProfile':{'type':'RuntimeDefault'}},'containers':[container]}}


def original(value,uid,nonce=None):
    meta=(value or {}).get('metadata',{})
    if (not uid or meta.get('uid')!=uid or meta.get('deletionTimestamp')
            or nonce and meta.get('annotations',{}).get('ai-sdlc.io/network-probe-nonce')!=nonce):
        raise ValueError('Original live resource identity required')
    return value


def validate_pod(value,expected,uid,nonce):
    original(value,uid,nonce)
    if value['metadata'].get('labels',{})!=expected['metadata']['labels']:
        raise ValueError('Original probe labels changed')
    spec=value.get('spec',{});wanted=expected['spec']
    if (spec.get('automountServiceAccountToken') is not False or spec.get('initContainers')
            or spec.get('ephemeralContainers') or spec.get('volumes') or spec.get('hostNetwork')
            or spec.get('hostPID') or spec.get('hostIPC') or len(spec.get('containers',[]))!=1):
        raise ValueError('Original restricted probe shape changed')
    actual=spec['containers'][0];desired=wanted['containers'][0]
    if (any(actual.get(k)!=desired[k] for k in ('name','image','command','securityContext'))
            or actual.get('args') or actual.get('env') or actual.get('envFrom') or actual.get('volumeMounts')
            or spec.get('securityContext')!=wanted['securityContext']):
        raise ValueError('Original probe execution changed')
    return value


def execute_probe(observe,expected,uid,nonce,execute):
    validate_pod(observe(),expected,uid,nonce)
    result=outcome(execute())
    validate_pod(observe(),expected,uid,nonce)
    return result


def cleanup_all(entries,remove,report,save):
    failed=False
    for entry in reversed(entries):
        result={'kind':entry[0],'name':entry[1],'uid':entry[3],'removed':False}
        try:
            remove(entry)
            result['removed']=True
        except (ValueError,OSError,subprocess.SubprocessError) as error:
            result['failure']=type(error).__name__
            failed=True
        report['cleanup'].append(result)
        try:save()
        except OSError:failed=True
    report['cleanupComplete']=not failed
    if failed:raise ValueError('Probe cleanup incomplete; preserve refused resources for inspection')


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cluster',required=True)
    parser.add_argument('--provider',choices=('auto','podman','docker'),default='auto')
    parser.add_argument('--output',type=Path,required=True)
    args=parser.parse_args(argv)
    if not re.fullmatch('[a-z][a-z0-9-]{0,35}',args.cluster):raise ValueError('Original local cluster required')
    args.output.mkdir(mode=0o700,parents=True,exist_ok=False)
    state=Path(os.environ.get('XDG_STATE_HOME',Path.home()/'.local/state'))/'ai-sdlc'/args.cluster
    private_directory(state)
    report={'version':1,'status':'running','workflowEvidence':False,'applicationWrites':False,'checks':[],'cleanup':[]}
    def save():platform.write_record(args.output/'report.json',report)
    save()
    try:
        with ExitStack() as stack:
            platform.lock_state(state,stack)
            provider=platform.select_provider(args.provider,state,args.cluster)
            binaries=install.tools(Path(os.environ.get('XDG_CACHE_HOME',Path.home()/'.cache'))/'ai-sdlc/tools')
            kubectl=[str(binaries['kubectl']),'--kubeconfig',str(state/'kubeconfig'),'--context','kind-'+args.cluster,'--request-timeout=10s']
            call_deadline=time.monotonic()+600
            def call(arguments,value=None,check=True):
                remaining=call_deadline-time.monotonic()
                if remaining<=0:raise ValueError('Probe operation deadline reached')
                r=subprocess.run(kubectl+arguments,input=json.dumps(value) if value is not None else None,text=True,capture_output=True,timeout=min(100,remaining))
                if check and r.returncode:raise ValueError('Bounded probe Kubernetes operation failed')
                if len(r.stdout)>4*1024*1024:raise ValueError('Bounded probe observation exceeded')
                return r
            def get(kind,name,namespace=None):
                raw=call(['get',kind,name,*(['-n',namespace] if namespace else []),'--ignore-not-found','-o','json']).stdout
                return json.loads(raw) if raw.strip() else None
            config=prepared_configuration.Configuration(install.run,kubectl,binaries,provider,args.cluster,state,{})
            if not config.record or config.record.get('phase')!='configured-paused':raise ValueError('Complete paused installation required')
            original_hash=hashlib.sha256(config.path.read_bytes()).hexdigest()
            observed=config.guard(inspect=True)
            description=config.record[config.current]
            if description['values'].get('release',{}).get('enabled') is not True:raise ValueError('Installed release capability required')
            target=json.loads(description['values']['release']['targetJSON'])
            if (target.get('clusterUID')!=observed['clusterUID'] or target.get('namespace')!='ai-sdlc-production'
                    or target.get('deployment')!='saferus-console' or target.get('service')!='saferus-console'
                    or target.get('tlsHostname')!='saferus-console' or target.get('expiresAt',0)<=int(time.time()*1000)):
                raise ValueError('Current original approved console target required')
            ns=get('namespace','ai-sdlc-production');runtime_ns=get('namespace',platform.NAMESPACE)
            if ns['metadata']['uid']!=target['namespaceUID'] or runtime_ns['metadata']['uid']!=observed['namespaceUID']:raise ValueError('Original namespaces replaced')
            deployment=get('deployment','saferus-console','ai-sdlc-production');service=get('service','saferus-console','ai-sdlc-production')
            if (deployment['metadata']['uid']!=target['deploymentUID'] or service['metadata']['uid']!=target['serviceUID']
                    or deployment['status'].get('readyReplicas')!=1 or deployment['spec']['template']['spec']['containers'][0]['image']!=target['baselineImage']
                    or service['spec'].get('selector')!={'app':'saferus-console'} or service['spec'].get('type')!='ClusterIP'
                    or len(service['spec'].get('ports',[]))!=1 or service['spec']['ports'][0]['port']!=8443):
                raise ValueError('Original ready baseline target changed')
            network=get('networkpolicy','ai-sdlc-deployer',platform.NAMESPACE)
            if not network or not get('networkpolicy','ai-sdlc-deployer-console','ai-sdlc-production'):raise ValueError('Installed release policies required')
            labels=network['spec']['podSelector']['matchLabels']
            if labels.get('app.kubernetes.io/component')!='deployer':raise ValueError('Closed deployer selector required')
            images=description['images'];image=images['image']['repository']+'@'+images['components']['worker']['digest']
            if not re.fullmatch(r'quay.io/os-santiago/ai-sdlc@sha256:[a-f0-9]{64}',image):raise ValueError('Original pinned probe-capable worker image required')
            nonce=secrets.token_hex(24);foreign='ai-sdlc-net-'+nonce[:12];allowed='release-net-'+nonce[:12]
            created=[];pod_intents={}
            report['fixture']={'nonce':nonce,'namespace':foreign,'runtimePod':allowed,'resources':[]}
            save()
            controllers=json.loads(call(['get','deployments','-n',platform.NAMESPACE,'-o','json']).stdout)['items']
            paused={obj['metadata']['name']:obj['metadata']['uid'] for obj in controllers}
            policies=[network,get('networkpolicy','ai-sdlc-deployer-console','ai-sdlc-production')]
            def fence():
                if hashlib.sha256(config.path.read_bytes()).hexdigest()!=original_hash:raise ValueError('Original configuration changed')
                current=json.loads(call(['get','deployments','-n',platform.NAMESPACE,'-o','json']).stdout)['items']
                if {obj['metadata']['name']:obj['metadata']['uid'] for obj in current}!=paused or any(obj['spec'].get('replicas',1)!=0 or obj['metadata'].get('deletionTimestamp') for obj in current):
                    raise ValueError('Runtime is no longer its original paused installation')
                for policy in policies:
                    value=original(get('networkpolicy',policy['metadata']['name'],policy['metadata']['namespace']),policy['metadata']['uid'])
                    if value['spec']!=policy['spec']:raise ValueError('Installed network policy changed')
                original(get('namespace','kube-system'),target['clusterUID'])
                original(get('namespace',platform.NAMESPACE),observed['namespaceUID'])
                original(get('namespace','ai-sdlc-production'),target['namespaceUID'])
                original(get('service','saferus-console','ai-sdlc-production'),target['serviceUID'])
                for kind,name,namespace,uid in created:
                    if kind=='Namespace' and uid:original(get(kind,name),uid,nonce)
            def baseline():
                fence()
                d=original(get('deployment','saferus-console','ai-sdlc-production'),target['deploymentUID'])
                v=original(get('service','saferus-console','ai-sdlc-production'),target['serviceUID'])
                specs=build_runtime_install.component_call(provider,images['image']['repository']+'@'+images['migrator']['digest'],
                    'describe-release-specs',{'deployment':d,'service':v})
                if any(specs.get(field)!=target.get(field) for field in ('deploymentSpecSHA256','serviceSpecSHA256','baselineImage')):
                    raise ValueError('Approved original console specifications changed')
                sources=config.record['sources']['release-original-identities']
                for name,field in (('saferus-console-tls','tlsSecretUID'),('ai-sdlc-release-reader','pullSecretUID')):
                    secret=original(get('secret',name,'ai-sdlc-production'),target[field])
                    identity={'uid':secret['metadata']['uid'],'sha256':bootstrap_prepare.digest({'type':secret['type'],
                        'immutable':secret.get('immutable',False),'data':secret.get('data',{})})}
                    if identity!=sources[field]:raise ValueError('Original installed release credential changed')
            baseline()
            def create(value):
                fence()
                meta=value['metadata'];kind=value['kind'];namespace=meta.get('namespace')
                # Record before POST so a lost response still leads to owner-fenced
                # cleanup; there is no second create or adoption as connectivity proof.
                created.append((kind,meta['name'],namespace,None))
                if kind=='Pod':pod_intents[(namespace,meta['name'])]=value
                intent={'kind':kind,'name':meta['name'],'namespace':namespace,'uid':None}
                report['fixture']['resources'].append(intent);save()
                result=call(['create','-f','-','-o','json'],value,check=False)
                actual=json.loads(result.stdout) if result.returncode==0 else get(kind,meta['name'],namespace)
                if actual and actual['metadata'].get('annotations',{}).get('ai-sdlc.io/network-probe-nonce')==nonce:
                    created[-1]=(kind,meta['name'],namespace,actual['metadata']['uid'])
                    intent['uid']=actual['metadata']['uid'];save()
                if result.returncode or created[-1][3] is None:raise ValueError('Probe creation was not acknowledged')
                return actual
            def probe(namespace,name,host,tls):
                fence()
                owned=next((entry for entry in created if entry[:3]==('Pod',name,namespace)),None)
                if not owned:raise ValueError('Original live probe Pod required')
                request={'host':host,'port':8443 if tls else 8080,'tls':tls,'ca':target['tlsCA'] if tls else None}
                return execute_probe(lambda:get('pod',name,namespace),pod_intents[(namespace,name)],owned[3],nonce,
                    lambda:call(['-n',namespace,'exec',name,'--','node','-e',CLIENT,json.dumps(request)],check=False))
            def expect(name,namespace,pod_name,host,tls,allowed):
                matches=0
                for _ in range(8):
                    matches=matches+1 if probe(namespace,pod_name,host,tls)==allowed else 0
                    if matches==2:report['checks'].append(name);save();return
                    time.sleep(1)
                raise ValueError('Actual release network boundary differs')
            try:
                create({'apiVersion':'v1','kind':'Namespace','metadata':{'name':foreign,
                    'annotations':{'ai-sdlc.io/network-probe-nonce':nonce},'labels':{'pod-security.kubernetes.io/enforce':'restricted'}}})
                create(pod(platform.NAMESPACE,allowed,image,dict(labels),nonce))
                create(pod(foreign,'client',image,dict(labels),nonce))
                create(pod(foreign,'control',image,{'ai-sdlc.io/probe':'control'},nonce,True))
                for namespace,name in ((platform.NAMESPACE,allowed),(foreign,'client'),(foreign,'control')):
                    call(['-n',namespace,'wait','--for=condition=Ready','pod/'+name,'--timeout=90s'])
                control_entry=next(e for e in created if e[:3]==('Pod','control',foreign))
                control=validate_pod(get('pod','control',foreign),pod_intents[(foreign,'control')],control_entry[3],nonce)['status']['podIP']
                host=service['spec']['clusterIP']
                expect('foreign-client-positive-control-before',foreign,'client',control,False,True)
                expect('deployer-to-original-console-tls-before',platform.NAMESPACE,allowed,host,True,True)
                expect('foreign-namespace-denied-same-deployer-labels',foreign,'client',host,True,False)
                expect('foreign-client-positive-control-after',foreign,'client',control,False,True)
                expect('deployer-to-original-console-tls-after',platform.NAMESPACE,allowed,host,True,True)
            finally:
                cleanup_deadline=time.monotonic()+180
                call_deadline=cleanup_deadline
                def remove(entry):
                    kind,name,namespace,uid=entry
                    if time.monotonic()>=cleanup_deadline:raise ValueError('Cleanup total deadline reached')
                    # The original cluster is required even for cleanup. Each
                    # namespace/Pod has its independent immutable UID fence.
                    original(get('namespace','kube-system'),target['clusterUID'])
                    if namespace==platform.NAMESPACE:original(get('namespace',namespace),observed['namespaceUID'])
                    if namespace==foreign:
                        owner=next(e for e in created if e[0]=='Namespace')
                        original(get('namespace',foreign),owner[3],nonce)
                    actual=get(kind,name,namespace)
                    if actual is None:return
                    original(actual,uid or actual['metadata']['uid'],nonce)
                    uid=actual['metadata']['uid']
                    if kind=='Namespace' and json.loads(call(['get','pods','-n',name,'-o','json']).stdout)['items']:
                        raise ValueError('Probe namespace contains unowned Pods; retain for inspection')
                    path=('/api/v1/namespaces/'+namespace+'/pods/'+name if kind=='Pod' else '/api/v1/namespaces/'+name)
                    call(['delete','--raw',path,'-f','-'],{'apiVersion':'v1','kind':'DeleteOptions','preconditions':{'uid':uid}})
                    call([*(['-n',namespace] if namespace else []),'wait','--for=delete',kind+'/'+name,'--timeout=30s'])
                cleanup_all(created,remove,report,save)
            created=[]
            call_deadline=time.monotonic()+180
            baseline()
            config.guard(inspect=True)
            after=get('deployment','saferus-console','ai-sdlc-production')
            if (after['metadata']['uid']!=deployment['metadata']['uid'] or after['spec']!=deployment['spec']
                    or get('service','saferus-console','ai-sdlc-production')['spec']!=service['spec']
                    or hashlib.sha256(config.path.read_bytes()).hexdigest()!=original_hash):raise ValueError('Original runtime/application configuration changed')
            report.update(status='passed',originalConfigurationUnchanged=True,probeImage=image,clusterUID=target['clusterUID'],deploymentUID=target['deploymentUID'])
            save()
    except Exception:
        report['status']='failed'
        save()
        raise
    print(json.dumps({'status':report['status'],'checks':len(report['checks']),'report':str(args.output/'report.json')}))
    return 0

if __name__=='__main__':
    try:raise SystemExit(main())
    except (ValueError,OSError,subprocess.SubprocessError) as error:raise SystemExit('Release network verification failed: '+type(error).__name__)
