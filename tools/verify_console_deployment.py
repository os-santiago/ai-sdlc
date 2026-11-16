#!/usr/bin/env python3
"""Actual isolated Kubernetes rollback/TLS fixture; no vault or workflow evidence."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import time
import yaml
import install


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--provider',choices=('podman','docker'),default='podman')
    p.add_argument('--output',type=Path,required=True)
    args=p.parse_args();args.output.mkdir(parents=True,mode=0o700,exist_ok=False)
    out=args.output.resolve();nonce=secrets.token_hex(8);cluster='ai-sdlc-console-'+nonce[:10]
    label='ai-sdlc.console-fixture';node=cluster+'-control-plane';cid=None;image_id=None
    names=[cluster+'-compile',cluster+'-test']
    env=dict(os.environ,KIND_EXPERIMENTAL_PROVIDER=args.provider,KUBECONFIG=str(out/'kubeconfig'))
    deadline=time.monotonic()+900
    report={'version':1,'scope':'isolated-kubernetes-console-rollback','status':'running','workflowEvidence':False,
        'vaultCryptographyValidated':False,'candidateSuccessfulRolloutValidated':False,'cluster':cluster,'stages':[]}
    def save():
        path=out/'report.json';path.write_text(json.dumps(report,indent=2)+'\n');path.chmod(0o600)
    def run(argv,*,data=None,limit=60,log=None):
        remaining=min(limit,deadline-time.monotonic())
        if remaining<=0:raise ValueError('Fixture deadline exceeded')
        result=subprocess.run([str(x) for x in argv],input=data,capture_output=True,timeout=remaining,env=env)
        if log is not None:log.write_bytes(result.stdout+result.stderr)
        if result.returncode:raise ValueError('Fixture operation failed: '+Path(str(argv[0])).name)
        return result.stdout
    def stage(name):report['stages'].append(name);save();print(name,flush=True)
    tools=install.tools(Path.home()/'.cache/ai-sdlc/tools')
    lock=json.loads((install.ROOT/'tools/runtime-validation.lock.json').read_text())
    kubectl=[tools['kubectl'],'--kubeconfig',out/'kubeconfig','--request-timeout=10s']
    def get(kind,name,namespace=None):
        return json.loads(run(kubectl+['get',kind,name,*(['-n',namespace] if namespace else []),'-o','json']))
    def create(obj):
        obj.setdefault('metadata',{}).setdefault('annotations',{})[label]=nonce
        return json.loads(run(kubectl+['create','-f','-','-o','json'],data=json.dumps(obj).encode()))
    try:
        stage('compile-trusted-synthetic-server')
        run([args.provider,'run','--rm','--name',names[0],'--network=none','--label',label+'='+nonce,
            '-e','CGO_ENABLED=0','-v',str(install.ROOT/'tools/fixtures/console')+':/fixture:ro,z',
            '-v',str(out)+':/output:z','-v',str(Path.home()/'.cache/ai-sdlc/validation/go/build')+':/root/.cache/go-build:z',
            lock['go'],'go','build','-trimpath','-ldflags=-s -w','-o','/output/console','/fixture/main.go'],limit=180)
        (out/'Containerfile').write_text('FROM scratch\nCOPY console /console\nUSER 1000:1000\nENTRYPOINT ["/console"]\nLABEL '+label+'="'+nonce+'"\n')
        tag='localhost/saferus-console:fixture-'+nonce
        run([args.provider,'build',*(['--platform=linux/amd64','--provenance=false'] if args.provider=='docker' else []),'-t',tag,'-f',out/'Containerfile',out],limit=120)
        item=json.loads(run([args.provider,'image','inspect',tag]))[0];image_id=item['Id']
        run([args.provider,'save','-o',out/'console.tar',tag],limit=60)
        stage('create-owned-disposable-kind')
        run([tools['kind'],'create','cluster','--name',cluster,'--config',install.LOCAL/'kind.yaml','--kubeconfig',out/'kubeconfig','--wait','120s'],limit=180)
        node_data=json.loads(run([args.provider,'inspect',node]))[0];cid=node_data['Id']
        if node_data['Config']['Labels'].get('io.x-k8s.kind.cluster')!=cluster:raise ValueError('Wrong fixture node owner')
        run([args.provider,'exec','-i',cid,'ctr','-n','k8s.io','images','import','--digests','-'],data=(out/'console.tar').read_bytes(),limit=120)
        inventory=run([args.provider,'exec',cid,'ctr','-n','k8s.io','images','ls']).decode().splitlines()
        matches=[line.split()[2] for line in inventory if line.split() and line.split()[0]==tag]
        if len(matches)!=1 or not re.fullmatch('sha256:[a-f0-9]{64}',matches[0]):raise ValueError('Fixture image digest unavailable')
        baseline='localhost/saferus-console@'+matches[0]
        if not any(line.split() and line.split()[0]==baseline for line in inventory):run([args.provider,'exec',cid,'ctr','-n','k8s.io','images','tag',tag,baseline])
        stage('install-isolated-console-fixture')
        run(['openssl','req','-x509','-newkey','ec','-pkeyopt','ec_paramgen_curve:P-256','-nodes','-days','2','-subj','/CN=console-fixture-ca','-keyout',out/'ca.key','-out',out/'ca.crt','-addext','basicConstraints=critical,CA:TRUE','-addext','keyUsage=critical,keyCertSign,cRLSign'])
        run(['openssl','req','-new','-newkey','ec','-pkeyopt','ec_paramgen_curve:P-256','-nodes','-subj','/CN=saferus-console','-keyout',out/'tls.key','-out',out/'tls.csr'])
        (out/'tls.ext').write_text('basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:saferus-console\n')
        run(['openssl','x509','-req','-in',out/'tls.csr','-CA',out/'ca.crt','-CAkey',out/'ca.key','-CAcreateserial','-days','1','-extfile',out/'tls.ext','-out',out/'tls.crt'])
        ns='ai-sdlc-production';name='saferus-console'
        namespace=create({'apiVersion':'v1','kind':'Namespace','metadata':{'name':ns}})
        create({'apiVersion':'v1','kind':'ServiceAccount','metadata':{'name':'saferus-client','namespace':ns},'automountServiceAccountToken':False})
        pull=create({'apiVersion':'v1','kind':'Secret','metadata':{'name':'ai-sdlc-release-reader','namespace':ns},'type':'kubernetes.io/dockerconfigjson','immutable':True,'data':{'.dockerconfigjson':base64.b64encode(b'{"auths":{}}').decode()}})
        tls_secret=create({'apiVersion':'v1','kind':'Secret','metadata':{'name':'saferus-console-tls','namespace':ns},'type':'kubernetes.io/tls','data':{key:base64.b64encode((out/key).read_bytes()).decode() for key in ('tls.crt','tls.key')}})
        service=create({'apiVersion':'v1','kind':'Service','metadata':{'name':name,'namespace':ns},'spec':{'selector':{'app':name},'ports':[{'name':'https','port':8443,'targetPort':'https'}]}})
        deployment=create({'apiVersion':'apps/v1','kind':'Deployment','metadata':{'name':name,'namespace':ns},'spec':{'replicas':1,'selector':{'matchLabels':{'app':name}},'template':{'metadata':{'labels':{'app':name}},'spec':{
            'serviceAccountName':'saferus-client','automountServiceAccountToken':False,'imagePullSecrets':[{'name':'ai-sdlc-release-reader'}],
            'securityContext':{'runAsNonRoot':True,'runAsUser':1000,'runAsGroup':1000,'fsGroup':1000,'seccompProfile':{'type':'RuntimeDefault'}},
            'containers':[{'name':'console','image':baseline,'imagePullPolicy':'IfNotPresent','ports':[{'name':'https','containerPort':8443}],
                'securityContext':{'readOnlyRootFilesystem':True,'allowPrivilegeEscalation':False,'capabilities':{'drop':['ALL']}},
                'resources':{'requests':{'cpu':'20m','memory':'16Mi'},'limits':{'cpu':'250m','memory':'64Mi'}},
                'volumeMounts':[{'name':'tls','mountPath':'/tls','readOnly':True}],
                'readinessProbe':{'httpGet':{'path':'/health/live','port':'https','scheme':'HTTPS'}}}],
            'volumes':[{'name':'tls','secret':{'secretName':'saferus-console-tls','defaultMode':288}}]}}}})
        run(kubectl+['rollout','status','deployment/'+name,'-n',ns,'--timeout=120s'],limit=140)
        pods=json.loads(run(kubectl+['get','pods','-n',ns,'-l','app='+name,'-o','json']))['items']
        if len(pods)!=1:raise ValueError('Fixture baseline Pod ambiguous')
        runtime=pods[0]['status']['containerStatuses'][0]['imageID'].split('@')[-1]
        if not re.fullmatch('sha256:[a-f0-9]{64}',runtime):raise ValueError('Runtime descriptor unavailable')
        def blob(digest):
            data=run([args.provider,'exec',cid,'ctr','-n','k8s.io','content','get',digest],limit=30)
            if len(data)>32*1024*1024 or 'sha256:'+hashlib.sha256(data).hexdigest()!=digest:raise ValueError('Fixture OCI content hash mismatch')
            return data
        root_doc=json.loads(blob(matches[0]));runtime_doc=json.loads(blob(runtime))
        manifest_doc=root_doc
        if runtime!=matches[0]:
            # Docker can retain a single-platform index as the image reference
            # and report its child manifest. OCI tar import can instead report
            # a wrapper index for the directly referenced manifest. Accept only
            # either exact one-edge relationship, verifying every blob hash.
            if root_doc.get('manifests'):
                desc=root_doc['manifests'];child=runtime;manifest_doc=runtime_doc
            else:
                desc=runtime_doc.get('manifests',[]);child=matches[0]
            child_raw=blob(child)
            if len(desc)!=1 or desc[0].get('digest')!=child or desc[0].get('size')!=len(child_raw) or desc[0].get('platform',{}).get('os')!='linux' or desc[0].get('platform',{}).get('architecture')!='amd64':raise ValueError('Runtime and reference do not share the sole approved manifest')
        manifest=manifest_doc;config=manifest['config'];config_raw=blob(config['digest'])
        platform=json.loads(config_raw)
        if platform.get('os')!='linux' or platform.get('architecture')!='amd64':raise ValueError('Fixture image platform differs')
        if len(config_raw)!=config['size']:raise ValueError('Fixture config size differs')
        for layer in manifest['layers']:
            if len(blob(layer['digest']))!=layer['size']:raise ValueError('Fixture layer size differs')
        report['baselineBinding']={'manifest':matches[0],'runtimeDescriptor':runtime,'configuration':config['digest'],'allContentHashesVerified':True}
        cfg=yaml.safe_load((out/'kubeconfig').read_text());user=cfg['users'][0]['user'];cluster_cfg=cfg['clusters'][0]['cluster']
        now=int(time.time()*1000)
        target={'version':'ai-sdlc.saferus-release-target/v1','scope':'kind-test-production','repository':'os-santiago/saferus','environment':ns,'clusterUID':get('namespace','kube-system')['metadata']['uid'],'namespaceUID':namespace['metadata']['uid'],'namespace':ns,'deployment':name,'deploymentUID':deployment['metadata']['uid'],'container':'console','deploymentSpecSHA256':'0'*64,'service':name,'serviceUID':service['metadata']['uid'],'serviceSpecSHA256':'0'*64,'pullSecret':'ai-sdlc-release-reader','pullSecretUID':pull['metadata']['uid'],'tlsSecretUID':tls_secret['metadata']['uid'],'tlsCA':(out/'ca.crt').read_text(),'tlsHostname':name,'baselineImage':baseline,'baselineRuntimeDigest':runtime,'rolloutSeconds':300,'smokeSeconds':30,'approvedAt':now-1000,'expiresAt':now+3600000}
        private={'Client':{'APIServer':'https://127.0.0.1:6443','CA':cluster_cfg['certificate-authority-data'],'ClientCertificate':user['client-certificate-data'],'ClientKey':user['client-key-data']},'Target':target,'Report':'/evidence/console-runtime-report.json'}
        private_path=out/'fixture-private.json';private_path.write_text(json.dumps(private));private_path.chmod(0o600)
        stage('actual-api-failed-candidate-owned-rollback-and-service-tls')
        result=run([args.provider,'run','--rm','--name',names[1],'--label',label+'='+nonce,'--network','container:'+cid,
            '-v',str(install.ROOT/'runtime/k8s')+':/src:ro,z','-v',str(out)+':/evidence:z',
            '-v',str(Path.home()/'.cache/ai-sdlc/validation/go/mod')+':/go/pkg/mod:z','-v',str(Path.home()/'.cache/ai-sdlc/validation/go/build')+':/root/.cache/go-build:z',
            '-w','/src','-e','AI_SDLC_CONSOLE_FIXTURE_JSON=/evidence/fixture-private.json',lock['go'],'go','test','-json','-count=1','-timeout=180s','./internal/podtransport','-run','^TestRealConsoleDeploymentRollback$'],limit=420,log=out/'test.log')
        (out/'test.log').write_bytes(result)
        proof=json.loads((out/'console-runtime-report.json').read_text())
        if proof.get('status')!='passed' or proof.get('workflowEvidence') is not False:raise ValueError('Fixture runtime proof incomplete')
        report['status']='passed'
    except BaseException as e:
        report['status']='failed';report['failureType']=type(e).__name__;raise
    finally:
        clean=True
        for name in names+[node]:
            try:
                found=subprocess.run([args.provider,'inspect',name],capture_output=True,timeout=10,check=False)
                if found.returncode==0:
                    item=json.loads(found.stdout)[0];labels=item.get('Config',{}).get('Labels',{});identity=item.get('Id','')
                    if (not re.fullmatch('[a-f0-9]{64}',identity) or name==node and (labels.get('io.x-k8s.kind.cluster')!=cluster or cid is not None and cid!=identity) or name!=node and labels.get(label)!=nonce):raise ValueError('Fixture cleanup ownership differs')
                    subprocess.run([args.provider,'rm','-f','-v',identity],capture_output=True,timeout=30,check=True)
                elif name in subprocess.run([args.provider,'ps','-a','--format','{{.Names}}'],capture_output=True,text=True,timeout=10,check=True).stdout.splitlines():raise ValueError('Fixture absence unproven')
            except BaseException:clean=False
        if image_id:
            try:
                item=json.loads(subprocess.run([args.provider,'image','inspect',image_id],capture_output=True,timeout=10,check=True).stdout)[0]
                if item.get('Labels',item.get('Config',{}).get('Labels',{})).get(label)!=nonce:raise ValueError('Fixture image owner differs')
                subprocess.run([args.provider,'rmi',image_id],capture_output=True,timeout=30,check=True)
            except BaseException:clean=False
        report['cleanupComplete']=clean
        if not clean:report['status']='failed'
        save()
    return 0 if report['status']=='passed' else 1
if __name__=='__main__':raise SystemExit(main())
