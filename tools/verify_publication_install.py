"""Validate publication installation in one fixed disposable kind cluster only."""
import argparse,json,pathlib,subprocess,sys,tempfile,re
sys.path.insert(0,str(pathlib.Path(__file__).resolve().parent))
import install,candidate_install,offline_runtime

def require(ok,message):
    if not ok:raise ValueError(message)

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--merge',action='store_true',help='Exercise explicit merge activation with frozen authority and zero workflow requests')
    parser.add_argument('--images',type=pathlib.Path,required=True)
    parser.add_argument('--github-auth',choices=['none','gh-cli'],default='none')
    parser.add_argument('--report',type=pathlib.Path,required=True)
    parser.add_argument('--codex-auth-file',type=pathlib.Path,required=True)
    args=parser.parse_args()
    cluster='ai-sdlc-publication-install-test';repository='os-santiago/homedir'
    state=pathlib.Path.home()/'.local/state/ai-sdlc'/cluster
    binaries=install.tools(pathlib.Path.home()/'.cache/ai-sdlc/tools')
    kubeconfig=state/'kubeconfig';k=[str(binaries['kubectl']),'--kubeconfig',str(kubeconfig)]
    def run(argv,value=None):
        result=subprocess.run([str(x) for x in argv],input=json.dumps(value) if value is not None else None,text=True,capture_output=True,timeout=650)
        if result.returncode:raise ValueError('Publication installation fixture command failed: '+result.stderr[-1500:])
        return result.stdout
    nodes=json.loads(run(k+['get','nodes','-o','json']))['items']
    require([n['metadata']['name'] for n in nodes]==[cluster+'-control-plane'],'Dedicated disposable cluster required')
    uid=json.loads(run(k+['get','namespace','kube-system','-o','json']))['metadata']['uid']
    lockpath=install.LOCAL/'images.lock.json';lock=install.image_lock(lockpath)
    images=json.loads(args.images.read_text())
    required={'intake','admission-controller','orchestrator','worker','release-manager','relay','verifier','migrator','model-mailbox'}
    require(set(images)==required and all(re.fullmatch(r'quay.io/os-santiago/ai-sdlc@sha256:[a-f0-9]{64}',v) for v in images.values()),'Exactly nine immutable local fixture images required')
    # Bootstrap policy with the same immutable migrator that the fixture deploys.
    lock['migrator']['digest']=images['migrator'].split('@')[1]
    public=install.configure_identities(k,'podman',lock,uid,repository,'ai-sdlc-production',freeze=True,candidate=True,verification=True,publication=True,merge=args.merge)
    if args.github_auth=='gh-cli':install.configure_github_client(k,uid,repository,False)
    public['intake']=install.shadow_intake_config(k,uid,repository)
    intake=json.loads(public['intake']['configJSON']);intake['triggerLabel']='ai-sdlc:publication-install-fixture';public['intake']['configJSON']=json.dumps(intake)
    profile=json.loads((install.LOCAL/'profiles/verification.json').read_text())
    public['contract']={'inline':profile['contract']['inline'].replace('ai-sdlc:shadow','ai-sdlc:publication-install-fixture')}
    values=candidate_install.desired_values(install.ROOT,lockpath,lock,verification=True,review=True,publication=True,merge=args.merge)
    values['candidate']['credentialRevision']=candidate_install.model_credentials(k,uid,args.codex_auth_file)
    candidate_install.review_credentials(k,uid)
    values['publication']['credentialRevision']=install.configure_publication_client(k,uid,repository,args.github_auth)
    offline_runtime.install(k,'podman',binaries['kind'],cluster,uid,pathlib.Path.home()/'.cache/ai-sdlc/tools/offline-runtime',images['worker'])
    values['candidate']['apiServerCIDRs']=candidate_install.api_cidrs(k)
    c=json.loads(values['candidate']['configJSON']);c['placement']['WorkerImage']=images['worker'];c['placement']['MailboxImage']=images['model-mailbox'];values['candidate']['configJSON']=json.dumps(c)
    c=json.loads(values['review']['configJSON']);c['workerImage']=images['worker'];c['mailboxImage']=images['model-mailbox'];values['review']['configJSON']=json.dumps(c)
    keys={'intake':'intake','admission-controller':'admissionController','orchestrator':'orchestrator','worker':'worker','release-manager':'releaseManager','relay':'relay'}
    values['components']={key:{'digest':images[name].split('@')[1]} for name,key in keys.items()}
    values['verifier']={'digest':images['verifier'].split('@')[1]};values['migrator']={'digest':images['migrator'].split('@')[1]}
    public.update(values);full_policy=json.loads(public['authority']['policyJSON'])
    def item(kind,name):return json.loads(run(k+['get',kind,name,'-n','ai-sdlc-runtime','-o','json']))
    credential_uid=item('secret','ai-sdlc-publication-client')['metadata']['uid']
    def db(query):return run(k+['-n','ai-sdlc-runtime','exec','statefulset/ai-sdlc-postgres','--','psql','-U','ai_sdlc','-d','ai_sdlc','-Atc',query]).strip()
    results=[]
    for enabled in [True,True,False,True]:
        public['merge' if args.merge else 'publication']['enabled']=enabled
        operation='merge-publication' if args.merge else 'publish-candidate'
        policy=dict(full_policy,grants=[g for g in full_policy['grants'] if enabled or g['operation']!=operation])
        if args.merge:policy['autonomy']='auto-merge-low' if enabled else 'auto-pr'
        public['authority']['policyJSON']=json.dumps(policy)
        with tempfile.NamedTemporaryFile(mode='w',suffix='.json') as file:
            json.dump(public,file);file.flush()
            run([binaries['helm'],'upgrade','--install','ai-sdlc',install.CHART,'--kubeconfig',kubeconfig,'--namespace','ai-sdlc-runtime','-f',install.LOCAL/'values.yaml','-f',install.LOCAL/'profiles/verification.json','-f',lockpath,'-f',file.name,'--wait=legacy','--wait-for-jobs','--timeout','8m'])
        expected='t' if enabled or args.merge else 'f'
        if args.merge:
            require(db("SELECT has_column_privilege('aisdlc_release_manager','merge_requests','state','UPDATE')")==('t' if enabled else 'f'),'Merge grant mismatch')
            require(db("SELECT has_column_privilege('aisdlc_release_manager','merge_requests','payload','UPDATE')")=='f','Mutable merge authority')
            require(db("SELECT has_column_privilege('aisdlc_release_manager','merged_records','merged_at','INSERT')")=='f','Backdatable merged fact')
            require(db('SELECT count(*) FROM merge_requests')=='0','Fixture created merge requests')
        require(db("SELECT has_table_privilege('aisdlc_release_manager','publication_requests','SELECT')")==expected,'Publication read grants mismatch')
        require(db("SELECT has_column_privilege('aisdlc_release_manager','publication_requests','state','UPDATE')")==expected,'Publication write grants mismatch')
        require(db("SELECT has_column_privilege('aisdlc_release_manager','publication_requests','payload','UPDATE')")=='f','Publisher can rewrite immutable payload')
        require(db("SELECT has_table_privilege('aisdlc_release_manager','review_runs','UPDATE')")=='f','Publisher can rewrite review proof')
        require(db('SELECT count(*) FROM execution_requests')=='0' and db('SELECT count(*) FROM publication_requests')=='0','Fixture created workflow requests')
        deployments=json.loads(run(k+['get','deployments','-n','ai-sdlc-runtime','-o','json']))['items']
        require(len(deployments)==7 and all(d['status'].get('readyReplicas')==1 for d in deployments),'Deployment readiness incomplete')
        for d in deployments:
            pod=d['spec']['template']['spec'];secrets={v.get('secret',{}).get('secretName') for v in pod.get('volumes',[])}
            if d['metadata']['name']=='ai-sdlc-release-manager':
                require(('ai-sdlc-publication-client' in secrets)==(enabled or args.merge),'Publisher credential mount mismatch')
                require(not pod.get('automountServiceAccountToken') and not any('projected' in v for v in pod.get('volumes',[])),'Publisher has Kubernetes token')
                require(len(pod['containers'])==1 and not secrets & {'ai-sdlc-codex-auth','ai-sdlc-model-client','ai-sdlc-review-model-client'},'Publisher has model capability')
            else:require('ai-sdlc-publication-client' not in secrets,'Publication credential escaped publisher')
        require(item('secret','ai-sdlc-publication-client')['metadata']['uid']==credential_uid,'Credential identity was replaced')
        routes=json.loads(item('configmap','ai-sdlc-config')['data']['AI_SDLC_RELAY_ROUTES'])
        require(('publish-candidate' in routes)==(enabled or args.merge) and 'verify-artifact' in routes,'Signed route mismatch')
        network=run(k+['get','networkpolicy','ai-sdlc-publication-github','-n','ai-sdlc-runtime','--ignore-not-found','-o','name']).strip()
        require(bool(network)==(enabled or args.merge),'Publisher egress policy mismatch')
        results.append({'mergeEnabled':enabled if args.merge else False,'publicationEnabled':enabled or args.merge,'readyDeployments':7,'privilegesMatched':True,'isolatedCredentialMounts':True,'workflowRequests':0})
        print(('merge' if args.merge else 'publication')+' enabled='+str(enabled)+' validated',flush=True)
    report={'scope':'isolated-merge-installation-fixture' if args.merge else 'isolated-publication-installation-fixture','realKubernetes':True,'realPostgres':True,'authorityFrozen':True,'autonomousE2E':False,'githubWrites':False,'workflowRequests':0,'locallyBuiltFixtureImages':True,'publishedImages':False,'credentialUIDPreserved':True,'scenarios':results}
    args.report.write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report),flush=True)
if __name__=='__main__':main()
