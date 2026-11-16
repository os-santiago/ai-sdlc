#!/usr/bin/env python3
"""Verify actual isolated registry signatures; never grant release authority."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import resource
import secrets
import subprocess
import tempfile
import time

import konflux_build_fixture as homedir_fixture
import saferus_build_fixture
import konflux_fixture_guard as guard
import konflux_provenance as provenance
import network_probe
import supply_chain_install


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--cluster',default='ai-sdlc-konflux')
    p.add_argument('--project',choices=['homedir','saferus'],default='homedir')
    p.add_argument('--build-report',type=Path,required=True)
    p.add_argument('--output',type=Path,required=True)
    args=p.parse_args()
    fixture = homedir_fixture if args.project == "homedir" else saferus_build_fixture
    if not re.fullmatch(r'[a-z][a-z0-9-]{0,35}',args.cluster):raise ValueError('Invalid cluster')
    args.output.mkdir(mode=0o700,parents=True,exist_ok=False)
    build=json.loads(args.build_report.read_text());results=build['results']
    digest=results['IMAGE_DIGEST'];repository=results['IMAGE_URL']
    if not re.fullmatch(r'sha256:[a-f0-9]{64}',digest) or repository!='registry-service.kind-registry/ai-sdlc-fixtures/'+args.project:raise ValueError('Unexpected isolated image identity')
    if results['CHAINS-GIT_URL']!=fixture.INPUTS['sourceURL'] or results['CHAINS-GIT_COMMIT']!=fixture.INPUTS['sourceSHA'] or results['RUN_UID']!=build['publishRunUID'] or results['DEFINITION_DIGEST']!=build['definitionDigest']:raise ValueError('Trusted publisher result identity mismatch')
    if fixture.definition_digest(build['publisherImage'],build['network']['deniedTargets'])!=build['definitionDigest']:raise ValueError('Build definition differs from this trusted checkout')
    image=repository+'@'+digest
    state,tools=guard.installation_paths(args.cluster);ownership=json.loads((state/'konflux-ownership.json').read_text())
    if ownership['cluster']!=args.cluster or ownership['scope']!='isolated-konflux':raise ValueError('Unexpected cluster ownership')
    kubectl=[str(tools/'kubectl-1.35.8'),'--kubeconfig',str(state/'kubeconfig')]
    deadline=supply_chain_install.Deadline(600)
    namespace='ai-sdlc-provenance-'+secrets.token_hex(6);uid=None
    report={'version':1,'image':image,'namespace':namespace,'sourceSHA':fixture.INPUTS['sourceSHA'],'definitionDigest':build['definitionDigest'],'publishRunUID':build['publishRunUID'],'status':'running','slsaLevelClaimed':False,'releaseAuthorized':False}
    def save():(args.output/'report.json').write_text(json.dumps(report,indent=2)+'\n')
    def call(a,value=None,check=True,limit=60):
        r=deadline.run(kubectl+a,input=json.dumps(value) if value is not None else None,capture_output=True,check=False,limit=limit)
        if check and r.returncode:raise ValueError('Provenance Kubernetes command failed: '+str(a[:3]))
        return r
    def get(kind,name,ns=namespace):return json.loads(call(['get',kind,name,'-n',ns,'-o','json']).stdout)
    def create(value):return json.loads(call(['create','-f','-','-o','json'],value).stdout)
    def execute(pod,command,label):
        # Bound local output as well as remote execution. Every verifier also
        # has explicit memory/CPU limits and no externally reachable egress.
        def bound():resource.setrlimit(resource.RLIMIT_FSIZE,(8<<20,8<<20))
        with tempfile.TemporaryFile() as stdout,tempfile.TemporaryFile() as stderr:
            r=deadline.run(kubectl+['exec','-n',namespace,pod,'--']+command,
                           stdout=stdout,stderr=stderr,check=False,limit=90,preexec_fn=bound)
            stdout.seek(0);stderr.seek(0);out=stdout.read((8<<20)+1);err=stderr.read((8<<20)+1)
        (args.output/(label+'.json')).write_bytes(out);(args.output/(label+'.log')).write_bytes(err)
        if len(out)>8<<20 or len(err)>8<<20:raise ValueError('Verifier output bound exceeded')
        return r.returncode,out
    try:
        if get('namespace','kube-system','default')['metadata']['uid']!=ownership['clusterUID']:raise ValueError('Cluster identity changed')
        ns=create({'apiVersion':'v1','kind':'Namespace','metadata':{'name':namespace,'labels':{'app.kubernetes.io/part-of':'ai-sdlc-provenance-fixture','pod-security.kubernetes.io/enforce':'restricted'}}});uid=ns['metadata']['uid'];report['namespaceUID']=uid;save()
        policy=fixture.policies(namespace)[1];policy['metadata']['name']='verify';policy['spec']['podSelector']={'matchLabels':{'ai-sdlc.role':'verify'}};policy['spec']['egress']=[fixture.PUBLIC_EGRESS[0],fixture.REGISTRY_EGRESS];create(policy)
        secret=get('secret','internal-registry-client-auth','kind-registry')
        create({'apiVersion':'v1','kind':'Secret','metadata':{'name':'registry-auth','namespace':namespace},'type':'kubernetes.io/dockerconfigjson','data':{'.dockerconfigjson':secret['data']['.dockerconfigjson']}})
        public=(args.build_report.parent/'cosign.pub').read_text()
        report['publicKeySHA256']=hashlib.sha256(public.encode()).hexdigest()
        with tempfile.TemporaryDirectory() as temp:
            key=Path(temp)/'test.key';key.touch(mode=0o600)
            deadline.run(['openssl','ecparam','-name','prime256v1','-genkey','-noout','-out',key],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,limit=10)
            wrong=deadline.run(['openssl','pkey','-in',key,'-pubout'],capture_output=True,limit=10).stdout
        create({'apiVersion':'v1','kind':'ConfigMap','metadata':{'name':'public-keys','namespace':namespace},'data':{'cosign.pub':public,'wrong.pub':wrong,'policy.json':'{"sources":[]}'}})
        for name,image_pin in [('cosign',fixture.INPUTS['cosignImage']),('conforma',fixture.INPUTS['conformaImage'])]:
            pod=network_probe.pod(namespace,name,image_pin);pod['metadata']['labels']['ai-sdlc.role']='verify';spec=pod['spec'];spec['activeDeadlineSeconds']=540;spec['securityContext']['fsGroup']=1000
            c=spec['containers'][0];c['command']=['/bin/sh','-c','sleep 540'];c['resources']={'requests':{'cpu':'100m','memory':'256Mi'},'limits':{'cpu':'2','memory':'1Gi'}}
            c['env']=[{'name':'HOME','value':'/tmp'},{'name':'DOCKER_CONFIG','value':'/auth'},{'name':'SSL_CERT_FILE','value':'/ca/ca-bundle.crt'}]
            spec['volumes']=[{'name':'auth','secret':{'secretName':'registry-auth','items':[{'key':'.dockerconfigjson','path':'config.json'}]}},{'name':'ca','configMap':{'name':'trusted-ca'}},{'name':'keys','configMap':{'name':'public-keys'}},{'name':'tmp','emptyDir':{'medium':'Memory','sizeLimit':'64Mi'}}]
            c['volumeMounts']=[{'name':n,'mountPath':'/'+n,'readOnly':n!='tmp'} for n in ('auth','ca','keys','tmp')]
            create(pod)
        call(['wait','-n',namespace,'--for=condition=Ready','pod/cosign','pod/conforma','--timeout=120s'],limit=130)
        base=['/usr/local/bin/cosign','verify','--key=/keys/cosign.pub','--insecure-ignore-tlog=true']
        rc,out=execute('cosign',base+[image],'image-signature')
        if rc:raise ValueError('Actual image signature validation failed')
        report['imageSignatureVerified']=True;save()
        attest=['/usr/local/bin/cosign','verify-attestation','--key=/keys/cosign.pub','--insecure-ignore-tlog=true','--type=slsaprovenance']
        rc,out=execute('cosign',attest+[image],'attestation')
        if rc:raise ValueError('Actual attestation signature validation failed')
        report['attestationSignatureVerified']=True;save()
        statement=provenance.verified_statement(out,results,build['publisherImage'])
        (args.output/'verified-payload.json').write_text(json.dumps(statement,indent=2)+'\n')
        report['signedIdentityBound']=True;save()
        rc,_=execute('cosign',[x.replace('/keys/cosign.pub','/keys/wrong.pub') for x in attest]+[image],'wrong-key')
        if rc==0:raise ValueError('Untrusted signing key was accepted')
        report['wrongKeyRejected']=True
        rc,_=execute('cosign',attest+[repository+'@sha256:'+'0'*64],'missing-digest')
        if rc==0:raise ValueError('Missing artifact was accepted')
        report['missingArtifactRejected']=True
        ec=['/usr/local/bin/ec','validate','image','--image='+image,'--public-key=/keys/cosign.pub','--policy=/keys/policy.json','--ignore-rekor','--output=json','--timeout=60s']
        rc,out=execute('conforma',ec,'conforma-signature-gate')
        report['conformaSignatureGate']={'exitCode':rc,'scope':'builtin image and attestation signature checks; no release/SLSA policy'}
        if rc or provenance.strict_json(out).get('success') is not True:raise ValueError('Conforma builtin signature gate failed')
        report['status']='isolated-signatures-and-identity-verified';save()
    except BaseException as exc:
        report['status']='failed';report['failureType']=type(exc).__name__;save();raise
    finally:
        deadline=supply_chain_install.Deadline(90)
        if uid:network_probe.cleanup_namespace(lambda a,v=None:call(a,v).stdout,namespace,uid)
        report['temporaryNamespaceRemoved']=True;save()
    return 0

if __name__=='__main__':
    try:raise SystemExit(main())
    except (ValueError,subprocess.SubprocessError,OSError) as exc:raise SystemExit(str(exc))
