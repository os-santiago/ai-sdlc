#!/usr/bin/env python3
"""Verify kubelet transport using the retained non-executable installation marker.

No artifact is uploaded, no application is changed, and no workflow is advanced.
An expected container creation failure is never sufficient evidence of a pull.
"""
import argparse
import base64
from contextlib import ExitStack,redirect_stdout
from datetime import datetime,timedelta,timezone
import io
import json
from pathlib import Path
import re
import secrets
import time

import fixture_cleanup
import install
import konflux_runtime_install as platform
import managed_registry_install as registry
import network_probe
import registry_node_install
import supply_chain_install


def frozen(deadline,kubectl):
    def bounded(argv,**kwargs):return deadline.run(argv,limit=kwargs.pop('timeout',20),**kwargs)
    platform.require_frozen(bounded,kubectl)


def original_namespace(get,namespace,uid,nonce):
    meta=get('namespace',namespace).get('metadata',{})
    if (not uid or meta.get('uid')!=uid or meta.get('name')!=namespace or meta.get('deletionTimestamp')
            or meta.get('labels',{}).get('ai-sdlc.pull-fixture')!=nonce):
        raise ValueError('Original pull fixture namespace changed')


def create_namespaced(get,create,value,namespace,uid,nonce):
    if value.get('metadata',{}).get('namespace')!=namespace:raise ValueError('Pull fixture namespace scope changed')
    original_namespace(get,namespace,uid,nonce)
    return create(value)


def event_proof(events,pod,image,node,positive):
    if not isinstance(events,list) or len(events)>64:raise ValueError('Bounded kubelet event inventory required')
    created=datetime.fromisoformat(pod['metadata']['creationTimestamp'].replace('Z','+00:00'))
    for event in events:
        involved=event.get('involvedObject',{});source=event.get('source',{})
        if (involved.get('uid')!=pod['metadata']['uid'] or involved.get('name')!=pod['metadata']['name']
                or involved.get('kind')!='Pod' or source.get('component')!='kubelet' or source.get('host')!=node):continue
        stamp=event.get('lastTimestamp') or event.get('eventTime')
        if not isinstance(stamp,str):continue
        try:at=datetime.fromisoformat(stamp.replace('Z','+00:00'))
        except ValueError:continue
        if at<created or at>datetime.now(timezone.utc)+timedelta(seconds=5):continue
        message=event.get('message','')
        if not isinstance(message,str) or len(message)>16384:continue
        if positive and event.get('reason')=='Pulled' and message.startswith('Successfully pulled image "'+image+'"'):
            return {'eventUID':event['metadata']['uid'],'podUID':pod['metadata']['uid'],'observedAt':stamp,'result':'pulled'}
        if not positive and event.get('reason')=='Failed' and image in message and '401 Unauthorized' in message:
            return {'eventUID':event['metadata']['uid'],'podUID':pod['metadata']['uid'],'observedAt':stamp,'result':'anonymous-denied'}
    return None


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--cluster',required=True)
    p.add_argument('--provider',choices=('podman','docker'),default='podman')
    p.add_argument('--registry-ca',required=True,type=Path)
    p.add_argument('--output',required=True,type=Path)
    args=p.parse_args()
    if not re.fullmatch(r'[a-z][a-z0-9-]{0,35}',args.cluster):raise ValueError('Original local cluster required')
    args.output.mkdir(parents=True,mode=0o700,exist_ok=False)
    # This status call is read-only and releases its lock before the fixture.
    with redirect_stdout(io.StringIO()):
        registry_node_install.main(['status','--cluster',args.cluster,'--provider',args.provider,'--registry-ca',str(args.registry_ca)])
    import os
    state_dir=Path(os.environ.get('XDG_STATE_HOME',Path.home()/'.local/state'))/'ai-sdlc'/args.cluster
    prior=(state_dir/'registry-node-trust.json').read_bytes()
    binaries=install.tools(Path(os.environ.get('XDG_CACHE_HOME',Path.home()/'.cache'))/'ai-sdlc/tools')
    kubectl=[str(binaries['kubectl']),'--kubeconfig',str(state_dir/'kubeconfig'),'--context','kind-'+args.cluster]
    nonce=secrets.token_hex(24);namespace='ai-sdlc-pull-'+nonce[:12];uid=None;attempted=False
    report={'version':1,'status':'running','namespace':namespace,'namespaceCreateNonce':nonce,'workflowEvidence':False,
        'imagePullValidated':False,'workloadExecutionValidated':False,'scope':'administrative-marker-transport-only'}
    def save():platform.write_record(args.output/'report.json',report)
    save()
    with ExitStack() as stack:
        platform.lock_state(state_dir,stack)
        if (state_dir/'registry-node-trust.json').read_bytes()!=prior:raise ValueError('Node trust changed during fixture acquisition')
        node_state=json.loads(prior);original=registry.read_record(state_dir/'managed-registry.json')
        deadline=supply_chain_install.Deadline(360)
        observer=registry.Installer(state_dir,original,kubectl,binaries,deadline,node_state['clusterUID'],read_only=True)
        def call(argv,value=None):
            limit=70 if argv[:2]==['wait','--for=delete'] else 20
            result=deadline.run(kubectl+argv,input=json.dumps(value) if value is not None else None,capture_output=True,limit=limit)
            if len(result.stdout)>1048576:raise ValueError('Fixture API output exceeds bound')
            return result.stdout
        def get(kind,name,ns=None):return json.loads(call(['get',kind,name,*(['-n',ns] if ns else []),'-o','json']))
        def fence():
            frozen(deadline,kubectl)
            if get('namespace','kube-system')['metadata']['uid']!=node_state['clusterUID']:raise ValueError('Fixture cluster replaced')
            current=get('node',args.cluster+'-control-plane')
            if current['metadata']['uid']!=node_state['nodeUID'] or current['status']['nodeInfo']['bootID']!=node_state['nodeBootID']:
                raise ValueError('Fixture original node changed')
            observer.fence(cold=False);observer.check_chart_objects()
        def create(value):
            def post(document):return json.loads(call(['create','-f','-','-o','json'],document))
            if value.get('kind')=='Namespace':return post(value)
            return create_namespaced(get,post,value,namespace,uid,nonce)
        def pod(name,authenticated,image):
            spec={'nodeName':args.cluster+'-control-plane','restartPolicy':'Never','activeDeadlineSeconds':120,
                'automountServiceAccountToken':False,'enableServiceLinks':False,'serviceAccountName':'default',
                'securityContext':{'runAsNonRoot':True,'runAsUser':65532,'runAsGroup':65532,'seccompProfile':{'type':'RuntimeDefault'}},
                'containers':[{'name':'marker','image':image,'imagePullPolicy':'Always','command':['/ai-sdlc-marker-has-no-executable'],
                    'resources':{'requests':{'cpu':'10m','memory':'16Mi'},'limits':{'cpu':'100m','memory':'32Mi'}},
                    'securityContext':{'readOnlyRootFilesystem':True,'allowPrivilegeEscalation':False,'capabilities':{'drop':['ALL']}}}]}
            if authenticated:spec['imagePullSecrets']=[{'name':'reader'}]
            return create({'apiVersion':'v1','kind':'Pod','metadata':{'name':name,'namespace':namespace,
                'labels':{'ai-sdlc.pull-fixture':nonce}},'spec':spec})
        def observe(original_pod,image,positive):
            end=time.monotonic()+80
            while time.monotonic()<end:
                frozen(deadline,kubectl)
                original_namespace(get,namespace,uid,nonce)
                current=get('pod',original_pod['metadata']['name'],namespace)
                if current['metadata']['uid']!=original_pod['metadata']['uid'] or current['metadata'].get('deletionTimestamp'):
                    raise ValueError('Original pull probe Pod changed')
                if current['spec']!=original_pod['spec']:raise ValueError('Original pull probe specification changed')
                events=json.loads(call(['get','events','-n',namespace,'--field-selector','involvedObject.uid='+current['metadata']['uid'],'-o','json']))['items']
                proof=event_proof(events,current,image,args.cluster+'-control-plane',positive)
                if proof:
                    original_namespace(get,namespace,uid,nonce)
                    if not positive:return proof
                    statuses=current.get('status',{}).get('containerStatuses',[])
                    if len(statuses)==1 and statuses[0].get('state',{}).get('waiting',{}).get('reason') in ('CreateContainerError','RunContainerError'):
                        proof['expectedNonExecutableMarker']=True
                        return proof
                time.sleep(1)
            raise ValueError('Kubelet did not provide the bounded expected pull evidence')
        try:
            fence()
            report['namespaceCreateIntent']=True;save();attempted=True
            ns=create({'apiVersion':'v1','kind':'Namespace','metadata':{'name':namespace,'labels':{
                'ai-sdlc.pull-fixture':nonce,'pod-security.kubernetes.io/enforce':'restricted'}}})
            uid=ns['metadata']['uid'];report['namespaceUID']=uid;save()
            create({'apiVersion':'networking.k8s.io/v1','kind':'NetworkPolicy','metadata':{'name':'deny-all','namespace':namespace},
                'spec':{'podSelector':{},'policyTypes':['Ingress','Egress'],'ingress':[],'egress':[]}})
            deadline.run(kubectl+['wait','-n',namespace,'--for=create','serviceaccount/default','--timeout=20s'],capture_output=True,limit=25)
            account=get('serviceaccount','default',namespace)
            if account.get('imagePullSecrets'):raise ValueError('Anonymous probe ServiceAccount has pull credentials')
            marker=original['conformance']['markerDigest']
            if not re.fullmatch('sha256:[a-f0-9]{64}',marker):raise ValueError('Original administrative marker digest required')
            image='registry-service.kind-registry/ai-sdlc-fixtures/saferus@'+marker
            report['image']=image;save()
            negative=pod('anonymous',False,image)
            report['anonymous']=observe(negative,image,False);save()
            original_namespace(get,namespace,uid,nonce)
            call(['delete','--raw','/api/v1/namespaces/'+namespace+'/pods/anonymous','-f','-'],
                {'apiVersion':'v1','kind':'DeleteOptions','preconditions':{'uid':negative['metadata']['uid']},'gracePeriodSeconds':0})
            source=get('secret','provenance-registry-reader','kind-registry')
            expected=original['objects']['Secret/provenance-registry-reader']
            value=original['custody']['reader']
            source_data=base64.b64encode(json.dumps(value,sort_keys=True,separators=(',',':')).encode()).decode()
            if (source['metadata']['uid']!=expected['uid'] or source['metadata'].get('annotations',{}).get(registry.NONCE)!=expected['nonce']
                    or source.get('immutable') is not True or source.get('data')!={'credential.json':source_data}):
                raise ValueError('Original reader credential custody changed')
            docker={'auths':{'registry-service.kind-registry':{'auth':base64.b64encode((value['username']+':'+value['password']).encode()).decode()}}}
            create({'apiVersion':'v1','kind':'Secret','metadata':{'name':'reader','namespace':namespace},'immutable':True,
                'type':'kubernetes.io/dockerconfigjson','data':{'.dockerconfigjson':base64.b64encode(json.dumps(docker).encode()).decode()}})
            positive=pod('reader',True,image)
            report['reader']=observe(positive,image,True);save()
            result=deadline.run([args.provider,'exec',node_state['nodeID'],'crictl','inspecti',image],capture_output=True,limit=15)
            if len(result.stdout)>1048576:raise ValueError('CRI image observation exceeds bound')
            actual=json.loads(result.stdout)['status']
            if image not in actual.get('repoDigests',[]) or not re.fullmatch('sha256:[a-f0-9]{64}',actual.get('id','')):
                raise ValueError('CRI image identity differs from retained marker')
            report['runtimeImageID']=actual['id'];report['cachedLayersMayBeReused']=True
            fence();original_namespace(get,namespace,uid,nonce)
            report['imagePullValidated']=True;report['status']='passed';save()
        except BaseException as exc:
            report['status']='failed';report['failureType']=type(exc).__name__;save();raise
        finally:
            deadline=supply_chain_install.Deadline(120)
            def cleanup(checkpoint):
                nonlocal uid
                if uid is None and attempted:
                    uid=fixture_cleanup.recover_namespace_uid(call,namespace,'ai-sdlc.pull-fixture',nonce)
                if uid:network_probe.cleanup_namespace(call,namespace,uid)
                report['temporaryNamespaceRemoved']=True;report['cleanupComplete']=True
            fixture_cleanup.finish_fixture(report,save,cleanup)
    print(json.dumps(report,indent=2))
    return 0


if __name__=='__main__':raise SystemExit(main())
