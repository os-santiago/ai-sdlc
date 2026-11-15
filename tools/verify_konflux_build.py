#!/usr/bin/env python3
"""Run an isolated two-stage Homedir/Konflux fixture; never advance live work."""
import argparse
import base64
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import time

import konflux_build_fixture as fixture
import network_probe
import supply_chain_install


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--cluster',default='ai-sdlc-konflux')
    p.add_argument('--publisher-image',required=True)
    p.add_argument('--output',type=Path,required=True)
    args=p.parse_args()
    if not re.fullmatch(r'[a-z][a-z0-9-]{0,35}',args.cluster) or not re.fullmatch(r'(localhost/ai-sdlc-artifact-builder|quay.io/os-santiago/ai-sdlc)@sha256:[a-f0-9]{64}',args.publisher_image):
        raise ValueError('Exact isolated cluster and publisher image digest required')
    args.output.mkdir(mode=0o700,parents=True,exist_ok=False)
    state=Path.home()/'.local/state/ai-sdlc'/args.cluster
    ownership=json.loads((state/'konflux-ownership.json').read_text())
    if ownership['cluster']!=args.cluster or ownership['scope']!='isolated-konflux':raise ValueError('Not an owned isolated Konflux installation')
    provider=ownership['provider'];node=args.cluster+'-control-plane'
    tools=Path.home()/'.cache/ai-sdlc/tools'
    kubectl=[str(tools/'kubectl-1.35.8'),'--kubeconfig',str(state/'kubeconfig')]
    deadline=supply_chain_install.Deadline(1800)
    token=secrets.token_hex(6);namespace='ai-sdlc-konflux-build-'+token;pv=namespace+'-volume'
    root='/tmp/ai-sdlc-507-'+token
    report={'version':1,'namespace':namespace,'inputs':fixture.INPUTS,'publisherImage':args.publisher_image,
            'definitionDigest':fixture.definition_digest(args.publisher_image),'autonomousE2E':False,
            'slsaLevelClaimed':False,'conformaAccepted':False,'status':'running'}
    uid=None;pv_uid=None;mounted=False;node_id=None
    def save():
        (args.output/'report.json').write_text(json.dumps(report,indent=2)+'\n')
    def call(a,value=None,check=True,timeout=100):
        r=deadline.run(kubectl+a,input=json.dumps(value) if value is not None else None,capture_output=True,check=False,limit=timeout)
        if check and r.returncode:raise ValueError('Fixture Kubernetes command failed: '+str(a[:4])+' '+r.stderr[-1500:])
        return r
    def get(kind,name=None,ns=namespace):
        return json.loads(call(['get',kind]+([name] if name else [])+(['-n',ns] if ns else [])+['-o','json']).stdout)
    def create(value):return json.loads(call(['create','-f','-','-o','json'],value).stdout)
    def node_call(a):
        observed=deadline.run([provider,'inspect',node,'--format','{{.Id}}'],capture_output=True,limit=15).stdout.strip()
        if observed!=node_id:raise ValueError('Owned kind node was replaced')
        return deadline.run([provider,'exec',node]+a,capture_output=True,limit=30)
    def collect_logs():
        try:
            pods=get('pods')['items']
            for pod in pods:
                name=pod['metadata']['name']
                for c in pod['spec']['containers']:
                    r=call(['logs','-n',namespace,name,'-c',c['name'],'--limit-bytes=8388608'],check=False,timeout=20)
                    (args.output/(name+'-'+c['name']+'.log')).write_text(r.stdout)
        except (ValueError,subprocess.SubprocessError):pass
    def wait_run(name,expected_uid):
        while True:
            run=get('pipelinerun',name)
            if run['metadata']['uid']!=expected_uid:raise ValueError('PipelineRun identity changed')
            conditions=run.get('status',{}).get('conditions',[])
            condition=next((c for c in conditions if c['type']=='Succeeded'),{})
            if condition.get('status') in ('True','False'):
                (args.output/(name+'.json')).write_text(json.dumps(run,indent=2)+'\n')
                if condition['status']!='True':raise ValueError('Pipeline failed: '+condition.get('message','unknown'))
                return run
            deadline.remaining();time.sleep(2)
    def terminated_writer(run):
        pods=get('pods')['items'];selected=[x for x in pods if x['metadata'].get('labels',{}).get('tekton.dev/pipelineRun')==run['metadata']['name']]
        if len(selected)!=1:raise ValueError('Expected one original Maven Pod')
        pod=selected[0]
        if pod['status'].get('phase')!='Succeeded' or pod['metadata'].get('deletionTimestamp'):raise ValueError('Maven writer is not terminated')
        statuses=pod['status'].get('containerStatuses',[])
        if not statuses or any(x.get('restartCount',0)!=0 or x.get('state',{}).get('terminated',{}).get('exitCode')!=0 for x in statuses):raise ValueError('Maven container did not terminate cleanly')
        if any(v.get('secret') or v.get('projected') for v in pod['spec'].get('volumes',[])):raise ValueError('Maven unexpectedly received a Secret/token volume')
        return {'podUID':pod['metadata']['uid'],'pod':pod['metadata']['name'],'containers':[{'name':x['name'],'containerID':x['containerID'],'exitCode':0} for x in statuses]}
    def probe_network():
        for role in ('build','control'):
            pod=network_probe.pod(namespace,'probe-'+role,fixture.INPUTS['builderImage'])
            pod['metadata']['labels']['ai-sdlc.role']=role
            pod['spec']['containers'][0]['command']=['/bin/bash','-c','sleep 240']
            create(pod)
        call(['wait','-n',namespace,'--for=condition=Ready','pod/probe-build','pod/probe-control','--timeout=90s'])
        service=get('service','registry-service','kind-registry')
        endpoints=get('endpoints','registry-service','kind-registry')
        api=get('service','kubernetes','default');api_endpoints=get('endpoints','kubernetes','default')
        targets=[(service['spec']['clusterIP'],443),(api['spec']['clusterIP'],443)]
        for ep in (endpoints,api_endpoints):
            for subset in ep['subsets']:
                targets.extend((a['ip'],port['port']) for a in subset['addresses'] for port in subset['ports'])
        if any(':' in host for host,_ in targets):raise ValueError('IPv6 fixture policy not reviewed')
        command='timeout 3 bash -c \'exec 3<>/dev/tcp/$1/$2\' probe "$1" "$2"'
        for host,port in targets:
            for role,expected in [('control',0),('build',124),('control',0)]:
                consecutive=0
                for attempt in range(20 if role=='build' else 1):
                    r=call(['exec','-n',namespace,'probe-'+role,'--','/bin/bash','-c',command,'probe',host,str(port)],check=False,timeout=10)
                    if r.returncode not in (0,124):raise ValueError('Network command failed; not a denial')
                    consecutive=consecutive+1 if r.returncode==expected else 0
                    if consecutive>=(2 if role=='build' else 1):break
                    time.sleep(1)
                else:raise ValueError('Network control failed for '+role+' '+host+':'+str(port)+' exit '+str(r.returncode))
        # DNS and HTTPS public access must still work under the build policy.
        r=call(['exec','-n',namespace,'probe-build','--','/bin/bash','-c','getent ahostsv4 github.com >/dev/null && curl --fail --silent --show-error --max-time 10 https://repo.maven.apache.org/maven2/ >/dev/null && test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token'],timeout=20)
        for role in ('build','control'):call(['delete','pod','probe-'+role,'-n',namespace,'--wait=true','--timeout=30s'])
        return {'deniedTargets':[{'host':h,'port':p} for h,p in targets],'positiveControls':True,'dnsAndPublicHTTPS':True,'serviceAccountTokenAbsent':True,'scope':'point-in-time-before-build'}
    try:
        if get('namespace','kube-system',None)['metadata']['uid']!=ownership['clusterUID']:raise ValueError('Cluster UID differs from ownership')
        node_info=json.loads(deadline.run([provider,'inspect',node],capture_output=True,limit=15).stdout)[0]
        if node_info['Config']['Labels'].get('io.x-k8s.kind.cluster')!=args.cluster:raise ValueError('Unexpected kind node')
        node_id=node_info['Id'];report['nodeID']=node_id
        ns=create({'apiVersion':'v1','kind':'Namespace','metadata':{'name':namespace,'labels':{'app.kubernetes.io/part-of':'ai-sdlc-konflux-fixture','pod-security.kubernetes.io/enforce':'restricted'}}});uid=ns['metadata']['uid'];report['namespaceUID']=uid;save()
        for policy in fixture.policies(namespace):create(policy)
        report['network']=probe_network();report['definitionDigest']=fixture.definition_digest(args.publisher_image,report['network']['deniedTargets']);save();print('Build-specific network controls passed',flush=True)
        # Dedicated bounded tmpfs is infrastructure. Workloads receive only a
        # claim-bound localPV, never arbitrary hostPath or mount privileges.
        node_call(['bash','-ceu','mkdir -m 700 "$1"; printf "%s" "$2" > "$1/owner"; mkdir "$1/volume"; mount -t tmpfs -o size=4294967296,nr_inodes=32768,mode=0770,uid=1000,gid=1000 "$3" "$1/volume"','fixture',root,uid,'ai-sdlc-507-'+token]);mounted=True
        mounted_info=json.loads(node_call(['findmnt','-J','-M',root+'/volume','-o','SOURCE,TARGET,FSTYPE,OPTIONS']).stdout)['filesystems'][0]
        if mounted_info['fstype']!='tmpfs' or mounted_info['source']!='ai-sdlc-507-'+token:raise ValueError('Dedicated bounded volume identity mismatch')
        report['volume']={'type':'dedicated-node-tmpfs-localPV','bytes':4294967296,'inodes':32768,'mount':mounted_info,'productionCSIIsolation':False};save()
        pvc=create({'apiVersion':'v1','kind':'PersistentVolumeClaim','metadata':{'name':'artifacts','namespace':namespace},'spec':{'storageClassName':'','volumeName':pv,'accessModes':['ReadWriteOnce'],'resources':{'requests':{'storage':'4Gi'}}}})
        volume=create({'apiVersion':'v1','kind':'PersistentVolume','metadata':{'name':pv,'labels':{'ai-sdlc.fixture':token}},'spec':{'capacity':{'storage':'4Gi'},'accessModes':['ReadWriteOnce'],'persistentVolumeReclaimPolicy':'Retain','storageClassName':'','local':{'path':root+'/volume'},'claimRef':{'namespace':namespace,'name':'artifacts','uid':pvc['metadata']['uid']},'nodeAffinity':{'required':{'nodeSelectorTerms':[{'matchExpressions':[{'key':'kubernetes.io/hostname','operator':'In','values':[node]}]}]}}}});pv_uid=volume['metadata']['uid']
        for name in ('build','publish'):create({'apiVersion':'v1','kind':'ServiceAccount','metadata':{'name':name,'namespace':namespace},'automountServiceAccountToken':False})
        build=create(fixture.pipeline_run(namespace,'homedir-maven','build',fixture.build_pipeline(report['network']['deniedTargets'])));report['buildRunUID']=build['metadata']['uid'];save()
        build=wait_run('homedir-maven',report['buildRunUID']);report['writer']=terminated_writer(build);collect_logs();save();print('Original Maven writer terminated successfully',flush=True)
        secret=get('secret','internal-registry-client-auth','kind-registry')
        create({'apiVersion':'v1','kind':'Secret','metadata':{'name':'registry-auth','namespace':namespace},'type':'kubernetes.io/dockerconfigjson','data':{'.dockerconfigjson':secret['data']['.dockerconfigjson']}})
        call(['patch','serviceaccount','publish','-n',namespace,'--type=merge','-p',json.dumps({'secrets':[{'name':'registry-auth'}]})])
        get('configmap','trusted-ca')
        output='registry-service.kind-registry/ai-sdlc-fixtures/homedir:'+token
        definition=fixture.publish_pipeline(args.publisher_image,report['definitionDigest'],output)
        publish=create(fixture.pipeline_run(namespace,'homedir-publish','publish',definition));report['publishRunUID']=publish['metadata']['uid'];save()
        published=wait_run('homedir-publish',report['publishRunUID']);report['results']={x['name']:x['value'] for x in published['status']['results']};collect_logs();save()
        print('Image published; waiting for Tekton Chains',flush=True)
        until=time.monotonic()+180
        while time.monotonic()<until:
            signed=get('pipelinerun','homedir-publish')
            if signed['metadata'].get('annotations',{}).get('chains.tekton.dev/signed')=='true':break
            time.sleep(2)
        else:raise ValueError('Chains did not report completed signing')
        (args.output/'signed-pipelinerun.json').write_text(json.dumps(signed,indent=2)+'\n')
        public=get('secret','public-key','tekton-pipelines')['data']['cosign.pub'];(args.output/'cosign.pub').write_bytes(base64.b64decode(public))
        report['chainsObservedSigned']=True;report['status']='published-awaiting-independent-provenance-validation';save()
    except BaseException as exc:
        report['status']='failed';report['failureType']=type(exc).__name__;save()
        raise
    finally:
        collect_logs()
        # Cleanup has its own bounded allowance so an exhausted execution budget
        # cannot turn a failed build into a leaked writable namespace silently.
        deadline=supply_chain_install.Deadline(120)
        cleanup=[]
        if uid:
            network_probe.cleanup_namespace(lambda a,v=None:call(a,v).stdout,namespace,uid);cleanup.append('namespace')
        if pv_uid:
            call(['delete','--raw','/api/v1/persistentvolumes/'+pv,'-f','-'],{'apiVersion':'v1','kind':'DeleteOptions','preconditions':{'uid':pv_uid}})
            call(['wait','--for=delete','pv/'+pv,'--timeout=30s']);cleanup.append('pv')
        if mounted:
            node_call(['bash','-ceu','test "$(cat "$1/owner")" = "$2"; test "$(findmnt -n -M "$1/volume" -o SOURCE)" = "$3"; umount "$1/volume"; rmdir "$1/volume"; rm "$1/owner"; rmdir "$1"','fixture',root,uid,'ai-sdlc-507-'+token]);cleanup.append('dedicated-tmpfs')
        report['cleanup']=cleanup;save()
    return 0

if __name__=='__main__':
    try:raise SystemExit(main())
    except (ValueError,subprocess.SubprocessError,OSError) as e:raise SystemExit(str(e))
