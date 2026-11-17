#!/usr/bin/env python3
"""Owned Kubernetes/API browser transport fixture; synthetic business API, never E2E."""
import argparse
import base64
import hashlib
import json
import ipaddress
import os
from pathlib import Path
import secrets
import subprocess
import time
import yaml
import browser_runtime
import browser_image
import browser_fixture_rbac
import browser_fixture_client
import browser_platform_install
import build_foundation_state
from types import SimpleNamespace
from release_target_oci import bounded_output


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--kubeconfig',type=Path,required=True)
    p.add_argument('--kubectl',type=Path,required=True)
    p.add_argument('--cluster',required=True)
    p.add_argument('--cluster-uid',required=True)
    p.add_argument('--image',required=True)
    p.add_argument('--binary',type=Path,required=True)
    p.add_argument('--helm',type=Path,required=True)
    p.add_argument('--provider',choices=('podman','docker'),default='podman')
    p.add_argument('--output',type=Path,required=True)
    p.add_argument('--platform-helper',help='Exercise the real browser platform installer on an owned fresh fixture cluster')
    args=p.parse_args();root=Path(__file__).resolve().parents[1]
    out=args.output.resolve();out.mkdir(mode=0o700,parents=True,exist_ok=False)
    nonce=secrets.token_hex(12);owner='ai-sdlc.browser-transport';owned={};cluster_owned=[];deadline=time.monotonic()+420;client=None
    k=[str(args.kubectl),'--kubeconfig',str(args.kubeconfig),'--request-timeout=10s']
    report={'scope':'real-kubernetes-browser-transport','autonomousE2E':False,'syntheticBusinessAPI':True,'status':'running','scenarios':[],'cleanup':[]}
    report['testBinarySHA256']=hashlib.sha256(args.binary.read_bytes()).hexdigest()
    report['fixtureSourceSHA256']=hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    def save(): (out/'report.json').write_text(json.dumps(report,indent=2)+'\n')
    def run(argv,data=None,limit=30):
        result=subprocess.run([str(x) for x in argv],input=data,capture_output=True,timeout=max(.1,min(limit,deadline-time.monotonic())))
        if result.returncode:raise ValueError('fixture operation failed: '+str(argv[0]))
        return result.stdout
    def rawget(kind,name,ns=None):return json.loads(run(k+['get',kind,name,*(['-n',ns] if ns else []),'-o','json']))
    def guard():
        if rawget('namespace','kube-system')['metadata']['uid']!=args.cluster_uid:raise ValueError('fixture cluster replaced')
        for name,uid in owned.items():
            m=rawget('namespace',name)['metadata']
            expected_owner=m.get('annotations',{}).get(build_foundation_state.ANNOTATION)==platform_nonce if name=='ai-sdlc-browser' and args.platform_helper else m.get('labels',{}).get(owner)==nonce
            if m['uid']!=uid or not expected_owner or m.get('deletionTimestamp'):raise ValueError('fixture namespace replaced')
    def get(kind,name,ns=None):guard();obj=rawget(kind,name,ns);guard();return obj
    def create(obj):
        guard();obj.setdefault('metadata',{}).setdefault('labels',{})[owner]=nonce
        result=json.loads(run(k+['create','-f','-','-o','json'],json.dumps(obj).encode()))
        if obj['kind'] in ('ClusterRole','ClusterRoleBinding'):cluster_owned.append((obj['kind'],result['metadata']['name'],result['metadata']['uid']))
        guard();return result
    def blob(digest,limit):
        current=json.loads(run([args.provider,'inspect',node]))[0]
        if current['Id']!=cid:raise ValueError('fixture node replaced')
        return bounded_output([args.provider,'exec',cid,'ctr','-n','k8s.io','content','get',digest],limit)
    try:
        guard();node=args.cluster+'-control-plane';metadata=json.loads(run([args.provider,'inspect',node]))[0];cid=metadata['Id']
        if metadata['Config']['Labels'].get('io.x-k8s.kind.cluster')!=args.cluster:raise ValueError('fixture node owner differs')
        if not args.platform_helper:report['runtime']=browser_runtime.install(k,args.provider,args.cluster,args.cluster_uid,bind_existing_configuration=True)
        digest=args.image.split('@')[-1]
        # Reference and running child/config identities are authenticated from
        # hash-checked bounded OCI objects, including index-vs-CRI differences.
        descriptors=browser_image.verify(digest,digest,blob)
        aliases={'browser':'quay.io/os-santiago/ai-sdlc@'+digest,'console':'localhost/saferus-console@'+digest}
        inventory=run([args.provider,'exec',cid,'ctr','-n','k8s.io','images','ls']).decode().splitlines()
        for alias in aliases.values():
            found=[line.split()[2] for line in inventory if line.split() and line.split()[0]==alias]
            if found and found!=[digest]:raise ValueError('foreign fixture image alias')
            if not found:run([args.provider,'exec',cid,'ctr','-n','k8s.io','images','tag',args.image,alias])
        client_namespace='ai-sdlc-browser-client-'+nonce[:10]
        for ns in (('ai-sdlc-production',client_namespace) if args.platform_helper else ('ai-sdlc-production','ai-sdlc-browser',client_namespace)):
            obj=create({'apiVersion':'v1','kind':'Namespace','metadata':{'name':ns,'labels':{'pod-security.kubernetes.io/enforce':'restricted'}}});owned[ns]=obj['metadata']['uid']
        for argv in (
            ['openssl','req','-x509','-newkey','ec','-pkeyopt','ec_paramgen_curve:P-256','-nodes','-days','1','-subj','/CN=browser-transport-fixture-ca','-keyout',out/'ca.key','-out',out/'ca.crt','-addext','basicConstraints=critical,CA:TRUE','-addext','keyUsage=critical,keyCertSign,cRLSign'],
            ['openssl','req','-new','-newkey','ec','-pkeyopt','ec_paramgen_curve:P-256','-nodes','-subj','/CN=saferus-console','-keyout',out/'tls.key','-out',out/'tls.csr']):run(argv)
        (out/'tls.ext').write_text('basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:saferus-console\n')
        run(['openssl','x509','-req','-in',out/'tls.csr','-CA',out/'ca.crt','-CAkey',out/'ca.key','-CAcreateserial','-days','1','-extfile',out/'tls.ext','-out',out/'tls.crt'])
        ns='ai-sdlc-production';name='saferus-console'
        create({'apiVersion':'v1','kind':'ServiceAccount','metadata':{'name':'saferus-client','namespace':ns},'automountServiceAccountToken':False})
        pull=create({'apiVersion':'v1','kind':'Secret','metadata':{'name':'ai-sdlc-release-reader','namespace':ns},'type':'kubernetes.io/dockerconfigjson','immutable':True,'data':{'.dockerconfigjson':base64.b64encode(b'{"auths":{}}').decode()}})
        tls=create({'apiVersion':'v1','kind':'Secret','metadata':{'name':'saferus-console-tls','namespace':ns},'type':'kubernetes.io/tls','data':{key:base64.b64encode((out/key).read_bytes()).decode() for key in ('tls.crt','tls.key')}})
        create({'apiVersion':'v1','kind':'ConfigMap','metadata':{'name':'synthetic-ui','namespace':ns},'immutable':True,'data':{'server.py':(root/'tools/fixtures/browser/server.py').read_text()}})
        service=create({'apiVersion':'v1','kind':'Service','metadata':{'name':name,'namespace':ns},'spec':{'selector':{'app':name},'ports':[{'name':'https','port':8443,'targetPort':'https'}]}})
        deployment=create({'apiVersion':'apps/v1','kind':'Deployment','metadata':{'name':name,'namespace':ns},'spec':{'replicas':1,'strategy':{'type':'Recreate'},'selector':{'matchLabels':{'app':name}},'template':{'metadata':{'labels':{'app':name}},'spec':{
            'serviceAccountName':'saferus-client','automountServiceAccountToken':False,'imagePullSecrets':[{'name':'ai-sdlc-release-reader'}],
            'terminationGracePeriodSeconds':1,
            'securityContext':{'runAsNonRoot':True,'runAsUser':1000,'runAsGroup':1000,'fsGroup':1000,'seccompProfile':{'type':'RuntimeDefault'}},
            'containers':[{'name':'console','image':aliases['console'],'imagePullPolicy':'IfNotPresent','command':['python','/fixture/server.py','baseline'],'ports':[{'name':'https','containerPort':8443}],
                'securityContext':{'readOnlyRootFilesystem':True,'allowPrivilegeEscalation':False,'capabilities':{'drop':['ALL']}},'resources':{'requests':{'cpu':'20m','memory':'64Mi'},'limits':{'cpu':'250m','memory':'256Mi'}},
                'volumeMounts':[{'name':'tls','mountPath':'/tls','readOnly':True},{'name':'fixture','mountPath':'/fixture','readOnly':True}],'readinessProbe':{'httpGet':{'path':'/health/live','port':'https','scheme':'HTTPS'}}}],
            'volumes':[{'name':'tls','secret':{'secretName':'saferus-console-tls','defaultMode':288}},{'name':'fixture','configMap':{'name':'synthetic-ui'}}]}}}})
        platform_nonce=None
        if not args.platform_helper:
            policies={}
            for obj in yaml.safe_load_all((root/'runtime/k8s/deploy/local/browser-platform/scope.yaml').read_text()):
                if obj['kind']=='Namespace':continue
                created=create(obj)
                if obj['kind']=='NetworkPolicy':policies[obj['metadata']['name']]=created['metadata']['uid']
            ca=create({'apiVersion':'v1','kind':'ConfigMap','metadata':{'name':'approved-console-ca','namespace':'ai-sdlc-browser'},'immutable':True,'data':{'ca.crt':(out/'ca.crt').read_text()}})
            original_node=get('node',node)
            profile={'nodeName':node,'nodeUID':original_node['metadata']['uid'],'version':'ai-sdlc.saferus-browser-profile/v1','clusterUID':args.cluster_uid,'namespaceUID':owned['ai-sdlc-browser'],'runtimeClassUID':report['runtime']['runtimeClassUID'],'caConfigMapUID':ca['metadata']['uid'],'denyPolicyUID':policies['deny-all'],'egressPolicyUID':policies['console-egress'],'consoleIngressPolicyUID':policies['ai-sdlc-browser-console'],'image':aliases['browser'],'runtimeDigest':descriptors['manifestDigest'],'caSHA256':hashlib.sha256((out/'ca.crt').read_bytes()).hexdigest(),'runtimeConfigurationSHA256':report['runtime']['configurationSHA256'],'baselineExpectation':'baseline-seconds','candidateExpectation':'readable-duration'}
        cfg=yaml.safe_load(args.kubeconfig.read_text());user=cfg['users'][0]['user'];cluster_cfg=cfg['clusters'][0]['cluster']
        for mode,expectation,accept in [('baseline','baseline-seconds',True),('readable','readable-duration',True),('wrong-ttl','baseline-seconds',False),('cleanup-only','baseline-seconds',False)]:
            guard()
            if mode!='baseline':
                observed=get('deployment',name,ns)
                patch=[{'op':'test','path':'/metadata/uid','value':deployment['metadata']['uid']},{'op':'test','path':'/metadata/resourceVersion','value':observed['metadata']['resourceVersion']},{'op':'replace','path':'/spec/template/spec/containers/0/command/2','value':'baseline' if mode=='cleanup-only' else mode}]
                run(k+['patch','deployment',name,'-n',ns,'--type=json','-p',json.dumps(patch)])
            run(k+['rollout','status','deployment/'+name,'-n',ns,'--timeout=90s'],limit=100)
            pods=json.loads(run(k+['get','pods','-n',ns,'-l','app='+name,'-o','json']))['items']
            if len(pods)!=1:raise ValueError('ambiguous fixture console Pod')
            image_id=pods[0]['status']['containerStatuses'][0]['imageID']
            browser_image.verify(digest,image_id.split('@')[-1],blob)
            if not args.platform_helper:profile['runtimeDigest']=image_id.split('@')[-1]
            now=int(time.time()*1000)
            target={'version':'ai-sdlc.saferus-release-target/v1','scope':'kind-test-production','repository':'os-santiago/saferus','environment':ns,'clusterUID':args.cluster_uid,'namespaceUID':owned[ns],'namespace':ns,'deployment':name,'deploymentUID':deployment['metadata']['uid'],'container':'console','deploymentSpecSHA256':'0'*64,'service':name,'serviceUID':service['metadata']['uid'],'serviceSpecSHA256':'0'*64,'pullSecret':'ai-sdlc-release-reader','pullSecretUID':pull['metadata']['uid'],'tlsSecretUID':tls['metadata']['uid'],'tlsCA':(out/'ca.crt').read_text(),'tlsHostname':name,'baselineImage':aliases['console'],'baselineRuntimeDigest':image_id.split('@')[-1],'rolloutSeconds':300,'smokeSeconds':30,'approvedAt':now-1000,'expiresAt':now+600000}
            if args.platform_helper and mode=='baseline':
                platform_state=out/'platform-state';platform_state.mkdir(mode=0o700)
                def product_run(argv,**kwargs):
                    return subprocess.run([str(x) for x in argv],text=True,check=True,**kwargs)
                def product_read(kind,name,namespace=None):
                    raw=run(k+['get',kind,name,*(['-n',namespace] if namespace else []),'--ignore-not-found','-o','json'])
                    return json.loads(raw) if raw.strip() else None
                config=SimpleNamespace(state_dir=platform_state,provider=args.provider,cluster=args.cluster,kubectl=k,
                    base_run=product_run,read=product_read,original={'original':{'nodeID':cid}})
                platform=browser_platform_install.Platform(config,target,aliases['browser'],args.platform_helper,guard,time.time()+300)
                try:profile=platform.run()
                finally:
                    custody=platform.record or {}
                    namespace=custody.get('resources',{}).get('v1/Namespace//ai-sdlc-browser',{})
                    if namespace.get('uid'):
                        platform_nonce=namespace['nonce'];owned['ai-sdlc-browser']=namespace['uid']
                    if (custody.get('runtime') or {}).get('state')=='complete':
                        runtime=custody['runtime']['observed'];report['runtime']=runtime
                        cluster_owned.append(('RuntimeClass','ai-sdlc-browser',runtime['runtimeClassUID']))
                report['platformInstallation']={'actualInstaller':True,'journalSHA256':hashlib.sha256(platform.path.read_bytes()).hexdigest(),
                    'helperImage':args.platform_helper,'sourceFilesSHA256':{path.name:hashlib.sha256(path.read_bytes()).hexdigest() for path in (root/'tools/browser_platform_install.py',root/'tools/browser_runtime.py',root/'tools/browser_profile_install.py')},
                    'scope':'browser infrastructure only; not the full runtime-upgrade CLI'}
                guard()
            profile['baselineExpectation']=expectation
            if mode=='baseline':
                api=get('service','kubernetes','default')['spec']['clusterIP']
                current_node=get('node',node)
                node_ips=[x['address'] for x in current_node['status']['addresses'] if x['type']=='InternalIP']
                if current_node['metadata']['uid']!=profile['nodeUID'] or len(node_ips)!=1 or any(ipaddress.ip_address(x).version!=4 for x in [api,*node_ips]):
                    raise ValueError('Original fixture API and node IPv4 identity required')
                api_cidrs=[x+'/32' for x in dict.fromkeys([api,*node_ips])]
                prefix='browser-'+nonce[:10]
                token=browser_fixture_rbac.install(run,k,args.helm,root,out,client_namespace,prefix,target,profile,create,api_cidrs)
                report['rbac']=browser_fixture_rbac.verify(args.kubectl,args.kubeconfig,out,token,node)
                client=browser_fixture_client.Client(args,node,cid,nonce,out,run,guard,create,get,report['testBinarySHA256'])
                client.prepare()
                report['client']={'image':client.image,'imageBinding':client.binding,'networkOrigin':'Pod with rendered deployer policies and ServiceAccount','privateInput':'scoped Secret, no ambient token mount'}
            private={'Client':{'APIServer':'https://'+api+':443','CA':cluster_cfg['certificate-authority-data'],'BearerToken':token},'Target':target,'Profile':profile,'Report':'/evidence/result.json','Accept':accept,'CleanupOnly':mode=='cleanup-only'}
            private_path=out/'private.json';private_path.write_text(json.dumps(private));private_path.chmod(0o600)
            guard()
            result=client.execute(k,client_namespace,prefix,mode,private);guard()
            report['scenarios'].append(result);save()
        report['status']='passed'
    except BaseException as exc:
        report['status']='failed';report['errorType']=type(exc).__name__;report['error']=str(exc) if isinstance(exc,ValueError) else 'fixture operation failed'
    finally:
        for kind,name,uid in reversed(cluster_owned):
            try:
                current=rawget(kind,name)
                expected_owner=current['metadata'].get('annotations',{}).get('ai-sdlc.io/cluster-uid')==args.cluster_uid if kind=='RuntimeClass' else current['metadata'].get('labels',{}).get(owner)==nonce
                if current['metadata']['uid']!=uid or not expected_owner:raise ValueError('cluster resource cleanup ownership')
                endpoint='/apis/node.k8s.io/v1/runtimeclasses/' if kind=='RuntimeClass' else '/apis/rbac.authorization.k8s.io/v1/'+('clusterroles/' if kind=='ClusterRole' else 'clusterrolebindings/')
                body={'apiVersion':'v1','kind':'DeleteOptions','preconditions':{'uid':uid}}
                run(k+['delete','--raw',endpoint+name,'-f','-'],json.dumps(body).encode())
                report['cleanup'].append({'kind':kind,'name':name,'uid':uid,'status':'removed'})
            except Exception:report['cleanup'].append({'kind':kind,'status':'failed'});report['status']='failed'
        for ns,uid in reversed(list(owned.items())):
            try:
                current=rawget('namespace',ns)
                expected_owner=current['metadata'].get('annotations',{}).get(build_foundation_state.ANNOTATION)==platform_nonce if ns=='ai-sdlc-browser' and args.platform_helper else current['metadata'].get('labels',{}).get(owner)==nonce
                if current['metadata']['uid']!=uid or not expected_owner:raise ValueError('cleanup ownership')
                body={'apiVersion':'v1','kind':'DeleteOptions','preconditions':{'uid':uid}}
                run(k+['delete','--raw','/api/v1/namespaces/'+ns,'-f','-'],json.dumps(body).encode())
                run(k+['wait','--for=delete','namespace/'+ns,'--timeout=60s'],limit=65)
                report['cleanup'].append({'namespace':ns,'status':'removed','uid':uid})
            except Exception:report['cleanup'].append({'namespace':ns,'status':'failed'});report['status']='failed'
        if client is not None:
            try:
                removed=client.close()
                if removed:report['cleanup'].append(removed)
            except Exception:report['cleanup'].append({'kind':'fixture-host-image','status':'failed'});report['status']='failed'
        save()
    print(json.dumps(report));return 0 if report['status']=='passed' else 1

if __name__=='__main__':raise SystemExit(main())
