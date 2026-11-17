"""Bounded initial configuration of an already empty, paused installation.

Only complete recorded before/after states may be resumed. Intermediate schema
or Kubernetes states stay excluded for the separately reviewed recovery path.
This never removes preparation custody or authorizes workflow execution.
"""
import base64
import copy
import hashlib
import json
from pathlib import Path
import tempfile
import time
import yaml

import bootstrap_prepare as bootstrap
import prepared_resources
import build_foundation_install
import build_foundation_state
import konflux_runtime_install as platform

VERSION='ai-sdlc.prepared-configuration/v1'


def same_foundation(before,after):
    """Configuration may change; original physical/database custody cannot."""
    for key in ('clusterUID','namespaceUID','nodeID','nodeUID','nodeBootID','databaseSecretSHA256'):
        if before.get(key)!=after.get(key):
            raise ValueError('Original prepared installation foundation changed')
    if before['resources']!=after['resources']:
        raise ValueError('Original prepared database or authority resource changed')
    for name,value in before['roleSecrets'].items():
        if after['roleSecrets'].get(name)!=value:
            raise ValueError('Original component database custody changed')
    for name,value in before['deployments'].items():
        if after['deployments'].get(name,{}).get('uid')!=value['uid']:
            raise ValueError('Original producer controller replaced')


class Configuration:
    def __init__(self,base_run,kubectl,binaries,provider,cluster,state_dir,request):
        self.base_run,self.kubectl,self.binaries=base_run,kubectl,binaries
        self.provider,self.cluster,self.state_dir=provider,cluster,state_dir
        self.path=state_dir/'prepared-configuration.json'
        self.deadline=time.monotonic()+1800
        self.request_digest=bootstrap.digest(request)
        original=build_foundation_install.bounded_json(state_dir/'bootstrap-preparation.json')
        if original.get('version')!=bootstrap.VERSION or original.get('phase')!='paused-empty':
            raise ValueError('Original successful empty preparation is required')
        self.original=original
        self.record=build_foundation_install.bounded_json(self.path,4*1024*1024) if self.path.exists() or self.path.is_symlink() else None
        if self.record is not None and (self.record.get('version')!=VERSION or self.record.get('originalSHA256')!=bootstrap.digest(original)):
            raise ValueError('Prepared configuration ownership changed')
        self.current=None

    def read(self,kind,name,namespace=platform.NAMESPACE):
        raw=self.base_run(self.kubectl+['get',kind,name,*(['-n',namespace] if namespace else []),
            '--ignore-not-found','-o','json'],capture_output=True,timeout=20).stdout
        if len(raw)>4*1024*1024:raise ValueError('Prepared observation exceeded bound')
        return json.loads(raw) if raw.strip() else None

    def inventory(self,kinds):
        raw=self.base_run(self.kubectl+['get',kinds,'-n',platform.NAMESPACE,'-o','json'],capture_output=True,timeout=20).stdout
        if len(raw)>4*1024*1024:raise ValueError('Prepared runtime inventory exceeded bound')
        return json.loads(raw)['items']

    def render(self,values):
        import install
        with tempfile.NamedTemporaryFile(mode='w',suffix='.json') as output:
            json.dump(values,output);output.flush()
            result=self.base_run([self.binaries['helm'],'template','ai-sdlc',str(install.CHART),'-n',platform.NAMESPACE,
                '-f',output.name],capture_output=True,timeout=30)
        if len(result.stdout)>4*1024*1024:raise ValueError('Prepared render exceeded bound')
        return [d for d in yaml.safe_load_all(result.stdout) if d]

    def observer(self,description):
        prep=bootstrap.Preparation(self.base_run,self.kubectl,self.provider,self.cluster,self.state_dir,description['images'])
        documents=description['documents']
        if description['values']['runtime']['mode']=='maintenance':
            prep.templates={d['metadata']['name']:d['spec']['template'] for d in documents if d.get('kind')=='Deployment'}
        else:
            prep.use_configured_render(documents)
        prep.expected=description.get('observed')
        return prep

    def save(self,candidate):
        platform.write_record(self.path,candidate)
        self.record=candidate

    def begin(self):
        if self.record is not None and self.record['phase']!='configured-paused':
            if self.record.get('requestSHA256')!=self.request_digest:
                raise ValueError('Incomplete prepared configuration cannot accept a different target')
            self.guard(inspect=True)
            self.resolve_secret_intents()
            return
        if self.record is None:
            report=build_foundation_install.bounded_json(self.state_dir/'installation.json')
            images=report.get('images')
            if bootstrap.digest(images)!=self.original['imagesSHA256']:
                raise ValueError('Original installed images differ from preparation custody')
            raw=self.base_run([self.binaries['helm'],'--kubeconfig',str(self.state_dir/'kubeconfig'),
                '-n',platform.NAMESPACE,'get','values','ai-sdlc','-o','json'],capture_output=True,timeout=30).stdout
            values=json.loads(raw)
            if values.get('runtime',{}).get('paused') is not True:
                raise ValueError('Prepared Helm desired state must remain paused')
            source={'images':images,'values':values,'documents':self.render(values),'observed':self.original['original'],
                'externalRuntime':prepared_resources.platform_resources(self)}
        else:
            self.check_secrets()
            source=copy.deepcopy(self.record['target'])
        resources=prepared_resources.observe(self,source,source.get('resourceUIDs'))
        source['resourceUIDs']=resources
        before=self.observer(source).inspect(paused=True)
        same_foundation(self.original['original'],before)
        secrets=self.secret_inventory()
        candidate={'version':VERSION,'originalSHA256':bootstrap.digest(self.original),
            'requestSHA256':self.request_digest,'phase':'preparing','source':source,'target':None,
            'resources':copy.deepcopy(self.record.get('resources',{})) if self.record else {},
            'privateSecrets':copy.deepcopy(self.record.get('privateSecrets',{})) if self.record else {},
            'originalSecrets':secrets,'sources':copy.deepcopy(self.record.get('sources',{})) if self.record else {}}
        self.save(candidate)
        self.current='source'

    def guard(self,inspect=False):
        if time.monotonic()>=self.deadline:
            raise ValueError('Prepared configuration deadline exceeded; preserve paused state')
        if self.record is None:raise ValueError('Prepared transition intent must precede effects')
        self.check_secrets()
        choices=['target','source'] if self.record.get('target') else ['source']
        for key in choices:
            description=self.record[key]
            try:
                if key=='source' and self.record.get('target'):
                    source_ids=set(prepared_resources.documents(description['documents']))
                    for identity,obj in prepared_resources.documents(self.record['target']['documents']).items():
                        if identity not in source_ids and self.read(obj['kind'],obj['metadata']['name'],obj['metadata'].get('namespace')) is not None:
                            raise ValueError('Partial prepared target resources exist; source is no longer complete')
                prepared_resources.observe(self,description,description.get('resourceUIDs'))
                prep=self.observer(description)
                observed=prep.inspect(paused=True) if inspect else prep.observe(paused=True)[0]
                same_foundation(self.original['original'],observed)
            except ValueError:
                continue
            self.current=key
            return observed
        raise ValueError('Prepared installation is neither its complete before nor after state; retain pause and original records')

    def set_target(self,values,images):
        documents=self.render(values)
        target={'images':images,'values':values,'documents':documents,'externalRuntime':self.record['source']['externalRuntime']}
        check=self.observer(target)  # Validate the closed, frozen target render.
        if check.configured_maps is None:raise ValueError('Configured capability target required')
        if self.record.get('target') is not None and {k:v for k,v in self.record['target'].items() if k not in ('observed','resourceUIDs')}!=target:
            raise ValueError('Prepared target changed during its recorded transition')
        if not set(self.record['source']['resourceUIDs'])<=set(prepared_resources.documents(documents)):
            raise ValueError('Prepared configuration cannot remove original Helm resources')
        previous=self.record.get('target')
        if previous is not None:
            target['resourceUIDs']=previous['resourceUIDs']
        else:
            target['resourceUIDs']={}
            for identity,obj in prepared_resources.documents(documents).items():
                uid=self.record['source']['resourceUIDs'].get(identity)
                if uid is None and self.read(obj['kind'],obj['metadata']['name'],obj['metadata'].get('namespace')) is not None:
                    raise ValueError('New prepared resource already exists without original target intent')
                target['resourceUIDs'][identity]=uid
        candidate=copy.deepcopy(self.record);candidate['target']=target;candidate['phase']='applying'
        self.save(candidate)

    def finish(self):
        self.resolve_secret_intents()
        self.check_secrets()
        resource_uids=prepared_resources.observe(self,self.record['target'],self.record['target']['resourceUIDs'])
        observed=self.observer(self.record['target']).inspect(paused=True)
        same_foundation(self.original['original'],observed)
        candidate=copy.deepcopy(self.record);candidate['target']['observed']=observed;candidate['target']['resourceUIDs']=resource_uids;candidate['phase']='configured-paused'
        self.save(candidate)

    def owned_secret(self,kubectl,name,cluster_uid):
        import install
        self.guard()
        # Existing installation readers retain their shape/key-pair checks.
        return install.owned_secret(kubectl,name,cluster_uid)

    def secret_inventory(self):
        import install
        raw=self.base_run(self.kubectl+['get','secrets','-n',platform.NAMESPACE,'-o','json'],capture_output=True,timeout=20).stdout
        if len(raw)>4*1024*1024:raise ValueError('Prepared credential inventory exceeded bound')
        allowed={'ai-sdlc-'+x for x in ('postgres-app','database-roles','github-client','publication-client',
            'codex-auth','model-client','review-model-client','provenance-registry')}
        allowed.update('ai-sdlc-'+prefix+'-'+role for role in install.ALL_ROLES for prefix in ('identity','database'))
        observations={}
        for item in json.loads(raw)['items']:
            name=item['metadata']['name']
            if item.get('type')=='helm.sh/release.v1' and name.startswith('sh.helm.release.v1.ai-sdlc.v'):
                continue
            if name not in allowed or item['metadata'].get('deletionTimestamp'):
                raise ValueError('Unexpected credential inventory in prepared runtime')
            observations[name]={'uid':bootstrap.Preparation.identity(item),
                'sha256':bootstrap.digest({'data':item.get('data',{}),'type':item.get('type'),'immutable':item.get('immutable',False),
                    'annotations':item['metadata'].get('annotations',{}),'labels':item['metadata'].get('labels',{}),
                    'ownerReferences':item['metadata'].get('ownerReferences',[])})}
        return observations

    def check_secrets(self):
        observed=self.secret_inventory()
        for name,expected in self.record['originalSecrets'].items():
            if observed.get(name)!=expected:
                raise ValueError('Original prepared credential changed')
        for name in (set(observed)-set(self.record['originalSecrets']))|set(self.record['privateSecrets']):
            desired=self.record['privateSecrets'].get(name)
            if desired is None:
                raise ValueError('Unrecorded credential appeared during preparation')
            key=build_foundation_state.identity(desired)
            intent=self.record['resources'].get(key)
            current=self.read('secret',name)
            if current is None and name==getattr(self,'active_creation',None) and (intent is None or intent.get('uid') is None):
                continue  # only the original in-process first POST; never restored from disk
            if (intent is None or current is None or current['metadata'].get('annotations',{}).get(build_foundation_state.ANNOTATION)!=intent['nonce']
                    or intent.get('uid') is not None and intent['uid']!=current['metadata']['uid']
                    or current['metadata'].get('deletionTimestamp')
                    or any(current['metadata'].get('annotations',{}).get(k)!=v for k,v in desired['metadata'].get('annotations',{}).items())
                    or build_foundation_state.body(current)!=build_foundation_state.body(desired)):
                raise ValueError('Prepared created credential custody changed')
        for name in self.record['privateSecrets']:
            desired=self.record['privateSecrets'][name]
            intent=self.record['resources'].get(build_foundation_state.identity(desired))
            if intent is not None and intent.get('uid') and name not in observed:
                raise ValueError('Original prepared credential is absent; refusing recreation')

    def read_resource(self,kubectl,kind,name):
        self.guard()
        return self.read(kind,name)

    def create_owned_secret(self,kubectl,name,values,cluster_uid):
        self.guard()
        if cluster_uid!=self.original['original']['clusterUID']:
            raise ValueError('Prepared credential scope changed')
        desired={'apiVersion':'v1','kind':'Secret','metadata':{'name':name,'namespace':platform.NAMESPACE,
            'labels':{'app.kubernetes.io/part-of':'ai-sdlc'},'annotations':{'ai-sdlc.io/cluster-uid':cluster_uid}},
            'type':'Opaque','data':{k:base64.b64encode(v.encode()).decode() for k,v in values.items()}}
        return self.ensure_secret(desired)

    def ensure_secret(self,desired):
        self.guard()
        if desired.get('kind')!='Secret' or desired['metadata'].get('namespace')!=platform.NAMESPACE:
            raise ValueError('Prepared credential destination must be its runtime namespace')
        name=desired['metadata']['name']
        candidate=copy.deepcopy(self.record)
        if name in candidate['privateSecrets'] and candidate['privateSecrets'][name]!=desired:
            raise ValueError('Prepared private credential changed; refusing rotation')
        initial=name not in candidate['privateSecrets'] and build_foundation_state.identity(desired) not in candidate['resources']
        candidate['privateSecrets'][name]=desired
        self.save(candidate)  # Private material and intent precede POST.
        def get(obj):
            self.guard();return self.read('secret',obj['metadata']['name'])
        def create(obj):
            self.guard()
            return json.loads(self.base_run(self.kubectl+['create','-f','-','-o','json'],input=json.dumps(obj),capture_output=True,timeout=20).stdout)
        if initial:self.active_creation=name
        try:
            return build_foundation_state.Resources(self.path,self.record,get,create).ensure(desired)
        finally:
            self.active_creation=None

    def resolve_secret_intents(self):
        self.check_secrets()
        for desired in self.record['privateSecrets'].values():
            intent=self.record['resources'].get(build_foundation_state.identity(desired))
            if intent is None or self.read('secret',desired['metadata']['name']) is None:
                raise ValueError('Prepared credential create outcome remains unresolved')
            def get(obj):return self.read('secret',obj['metadata']['name'])
            def no_create(obj):raise ValueError('Credential observation cannot recreate an absent original')
            build_foundation_state.Resources(self.path,self.record,get,no_create).ensure(desired)

    def bind_source(self,name,value):
        candidate=copy.deepcopy(self.record)
        sources=candidate.setdefault('sources',{})
        if name in sources and sources[name]!=value:
            raise ValueError('Original prepared source custody changed')
        sources[name]=value
        self.save(candidate)

    def run(self,argv,**kwargs):
        self.guard()
        kwargs['timeout']=min(kwargs.get('timeout',150),max(0.1,self.deadline-time.monotonic()))
        return self.base_run(argv,**kwargs)


def derive_bundle(path,lock,build_images,state_dir):
    """Advance only controller revision; retain the original physical profile."""
    original=build_foundation_install.bounded_json(path)
    foundation=build_foundation_install.bounded_json(state_dir/'build-foundation.json')
    if (foundation.get('version')!=build_foundation_install.VERSION or foundation.get('status')!='prepared'
            or bootstrap.digest(original)!=foundation.get('bundleSHA256')):
        raise ValueError('Original approved build bundle and foundation required')
    selected=build_foundation_install.bounded_json(build_images,4096)
    if (set(selected)!={'version','revision','publisherImage','controllerDigest'}
            or selected['version']!='ai-sdlc.build-images/v1' or selected['revision']!=lock['image']['tag']):
        raise ValueError('Reviewed new build controller must match the target runtime revision')
    derived=copy.deepcopy(original)
    derived.update(revision=lock['image']['tag'],controllerDigest=selected['controllerDigest'])
    # Reuse the strict portable input validator, retaining the original
    # publisher image, slots, namespace, boot and admission identities.
    import build_runtime_install
    with tempfile.NamedTemporaryFile(mode='w',suffix='.json') as stream:
        json.dump(derived,stream);stream.flush()
        build_runtime_install.load_bundle(stream.name,lock)
    if derived['profile'].get('publisherImage')!=foundation.get('publisherImage'):
        raise ValueError('Original build publisher identity changed')
    return derived,{'foundationSHA256':bootstrap.digest(foundation),'sourceBundleSHA256':bootstrap.digest(original)}


def merge_values(*sources):
    result={}
    for source in sources:
        for key,value in source.items():
            result[key]=merge_values(result.get(key,{}) if isinstance(result.get(key),dict) else {},value) if isinstance(value,dict) else copy.deepcopy(value)
    return result


def install_prepared(args,state,binaries,provider,execution_values):
    import install
    import candidate_install
    import build_runtime_install
    import github_merge_install
    import offline_runtime
    import provenance_runtime_install
    import release_runtime_install
    lock=install.image_lock(args.images)
    bundle,foundation=derive_bundle(args.build_bundle,lock,args.build_images,state)
    approved=build_foundation_install.bounded_json(args.provenance_trust)
    release=release_runtime_install.inputs(args.release_target,args.release_images,lock) if getattr(args,'release_target',None) is not None else None
    chart_digest=hashlib.sha256()
    for path in sorted(install.CHART.rglob('*')):
        if path.is_symlink():raise ValueError('Reviewed chart cannot contain symbolic links')
        if path.is_file():
            chart_digest.update(str(path.relative_to(install.CHART)).encode()+b'\0'+path.read_bytes()+b'\0')
    request={'chartSHA256':chart_digest.hexdigest(),'images':lock,'bundle':bundle,'foundation':foundation,'trust':approved,
        'repository':args.repository,'environment':args.environment,
        'localValues':yaml.safe_load((install.LOCAL/'values.yaml').read_text()),
        'profile':json.loads((install.LOCAL/'profiles/verification.json').read_text()),
        'execution':execution_values,'release':release}
    kubectl=[binaries['kubectl'],'--kubeconfig',str(state/'kubeconfig'),'--context','kind-'+args.cluster,'--request-timeout=10s']
    config=Configuration(install.run,kubectl,binaries,provider,args.cluster,state,request)
    config.begin()  # durable intent before credentials, sandbox or Helm effects
    cluster_uid=config.original['original']['clusterUID']
    platform.verify_cluster(install.run,kubectl,state,args.cluster,provider,cluster_uid)
    build_runtime_install.preflight(config.run,kubectl,provider,args.cluster,lock,bundle)
    provenance_values=provenance_runtime_install.configure(install.run,kubectl,provider,state,lock,bundle,
        args.provenance_trust,execution_values['verifier']['digest'],config)
    install.configure_github_client(kubectl,cluster_uid,args.repository,installer=config)
    intake=install.shadow_intake_config(kubectl,cluster_uid,args.repository,installer=config)
    public=install.configure_identities(kubectl,provider,lock,cluster_uid,args.repository,args.environment,
        freeze=True,candidate=True,verification=True,publication=True,merge=True,build=True,installer=config)
    public['intake']=intake
    execution=copy.deepcopy(execution_values)
    execution['candidate']['credentialRevision']=candidate_install.model_credentials(kubectl,cluster_uid,args.codex_auth_file,installer=config)
    candidate_install.review_credentials(kubectl,cluster_uid,installer=config)
    execution['publication']['credentialRevision']=install.configure_publication_client(kubectl,cluster_uid,args.repository,args.github_auth,installer=config)
    config.guard()
    execution['merge']['configJSON']=json.dumps(github_merge_install.reconcile(state,args.repository),sort_keys=True)
    config.guard()
    offline_runtime.install(kubectl,provider,binaries['kind'],args.cluster,cluster_uid,
        Path.home()/'.cache/ai-sdlc/tools',lock['image']['repository']+'@'+lock['components']['worker']['digest'])
    config.guard()
    execution['candidate']['apiServerCIDRs']=candidate_install.api_cidrs(kubectl)
    public.update(execution)
    public['build']=build_runtime_install.values(provider,lock,bundle,public,intake)
    public['provenance']=provenance_values
    if release is not None:
        public['release']=release_runtime_install.configure(install.run,kubectl,provider,lock,release,approved,config)
    public['runtime']={'mode':'verification','paused':True}
    desired=merge_values(request['localValues'],request['profile'],lock,public)
    config.set_target(desired,lock)
    config.guard(inspect=True)
    with tempfile.NamedTemporaryFile(mode='w',suffix='.json') as stream:
        json.dump(desired,stream);stream.flush()
        config.run([binaries['helm'],'upgrade','--install','ai-sdlc',str(install.CHART),
            '--kubeconfig',str(state/'kubeconfig'),'--kube-context','kind-'+args.cluster,
            '--namespace',platform.NAMESPACE,'-f',stream.name,'--wait=legacy','--wait-for-jobs','--timeout',args.timeout],timeout=900)
    build_runtime_install.preflight(config.run,kubectl,provider,args.cluster,lock,bundle)
    final_provenance=provenance_runtime_install.configure(install.run,kubectl,provider,state,lock,bundle,args.provenance_trust,execution['verifier']['digest'],config)
    if final_provenance!=public['provenance']:
        raise ValueError('Provenance installation changed during Helm application')
    if release is not None and release_runtime_install.configure(install.run,kubectl,provider,lock,release,approved,config)!=public['release']:
        raise ValueError('Release target changed during prepared installation')
    config.finish()
    report={'cluster':args.cluster,'platform':'konflux','mode':'verification','configuration':'prepared-paused',
        'namespace':platform.NAMESPACE,'installationReady':False,
        'configurationReady':True,'runtimeReadinessValidated':False,'runtimePaused':True,'innerFrozen':True,'outerFrozen':True,
        'deliveryEnabled':False,'issueIntakeEnabled':False,'liveWorkflowAdvanced':False,
        'capabilitiesConfigured':['verification','review','publication','merge','build','provenance']+(['release'] if release else []),
        'repository':args.repository,'environment':args.environment,'images':lock}
    platform.write_record(state/'installation.json',report)
    print(json.dumps(report,indent=2))
    return 0
