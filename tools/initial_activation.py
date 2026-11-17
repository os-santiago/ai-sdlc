"""Initial, finite activation of a complete empty installation; issue-only ingress."""
import argparse
import copy
from contextlib import ExitStack
import json
import hashlib
from pathlib import Path
import tempfile
import time

import bootstrap_prepare as bootstrap
import build_foundation_install as files
import build_runtime_install
import install
import konflux_runtime_install as platform
import prepared_configuration
import prepared_resources
import provenance_runtime_install
import release_runtime_install
import release_target_install

VERSION='ai-sdlc.initial-activation/v1'
PHASES=('prepared','warming','armed','active')


def chart_digest():
    digest=hashlib.sha256()
    for path in sorted(install.CHART.rglob('*')):
        if path.is_symlink():raise ValueError('Activation chart cannot contain symbolic links')
        if path.is_file():digest.update(str(path.relative_to(install.CHART)).encode()+b'\0'+path.read_bytes()+b'\0')
    return digest.hexdigest()


def activation_expiry(values,now):
    expiries=[json.loads(values[section][key]).get('expiresAt') for section,key in (('provenance','trustJSON'),('release','targetJSON'))]
    if any(type(value) is not int or value<=now*1000 for value in expiries):raise ValueError('Original approval window expired')
    return min(now+1800,*(value/1000 for value in expiries))


def phase_values(source,phase,compile_build):
    if phase not in PHASES[1:]:raise ValueError('Unknown activation target')
    result=copy.deepcopy(source)
    if (source.get('runtime',{}).get('paused') is not True or not all(source.get(k,{}).get('enabled') is True for k in ('build','provenance','release','review','publication','merge'))):
        raise ValueError('Complete paused release installation required')
    inner=json.loads(source['authority']['policyJSON']);outer=json.loads(source['build']['outerPolicyJSON'])
    installed=json.loads(source['build']['configJSON'])
    if inner.get('freeze') is not True or outer.get('freeze') is not True or installed.get('inner')!=inner or installed.get('outer')!=outer:
        raise ValueError('Original canonical frozen policies required')
    inner['freeze']=outer['freeze']=phase=='warming'
    request={key:copy.deepcopy(installed[key]) for key in ('profile','slots','issueMaxAgeSeconds')}
    request.update(inner=inner,outer=outer,intake=json.loads(source['intake']['configJSON']))
    compiled=compile_build(request)
    expected=dict(installed,inner=inner,outer=outer)
    if compiled!=expected:raise ValueError('Activation compiler changed original build authority')
    result['authority']['policyJSON']=json.dumps(inner,sort_keys=True)
    result['build']['outerPolicyJSON']=json.dumps(outer,sort_keys=True)
    result['build']['configJSON']=json.dumps(compiled,sort_keys=True)
    result['runtime'].update(paused=False,intakePaused=phase!='active')
    result['migrator']['enabled']=False
    return result


class Activation:
    def __init__(self,config):
        self.config=config;self.state=config.state_dir;self.binaries=config.binaries
        self.path=self.state/'initial-activation.json'
        self.original=copy.deepcopy(config.record)
        if self.original is None or self.original.get('phase')!='configured-paused':raise ValueError('Original complete prepared configuration required')
        if self.original.get('target',{}).get('values',{}).get('release',{}).get('enabled') is not True:
            raise ValueError('Initial activation requires the prepared release capability')
        self.original_hash=bootstrap.digest(self.original)
        self.record=files.bounded_json(self.path,16*1024*1024) if self.path.exists() or self.path.is_symlink() else None
        if self.record is not None and (self.record.get('version')!=VERSION or self.record.get('preparedSHA256')!=self.original_hash):
            raise ValueError('Initial activation custody changed')
        self.phase=None

    def unchanged(self):
        if bootstrap.digest(files.bounded_json(self.config.path,4*1024*1024))!=self.original_hash:
            raise ValueError('Original preparation record changed during activation')
        self.config.check_secrets()

    def describe(self,phase):
        return self.original['target'] if phase=='prepared' else self.record['targets'][phase]

    def observe(self,phase,empty=False):
        if phase not in PHASES or phase=='active' and empty:raise ValueError('Invalid activation observation')
        self.unchanged();description=self.describe(phase)
        prepared_resources.observe(self.config,description,self.original['target']['resourceUIDs'])
        observer=bootstrap.Preparation(self.config.base_run,self.config.kubectl,self.config.provider,self.config.cluster,self.state,description['images'])
        if phase=='prepared':observer.use_configured_render(description['documents'])
        else:observer.use_activation_render(description['documents'],phase)
        observed=observer.inspect(paused=phase=='prepared') if empty else observer.observe(paused=phase=='prepared')[0]
        prepared_configuration.same_foundation(self.original['target']['observed'],observed)
        self.phase=phase
        return observed

    def guard(self):
        if self.phase is None:raise ValueError('Activation phase has not been observed')
        return self.observe(self.phase)

    def bind_source(self,name,value):
        if self.original.get('sources',{}).get(name)!=value:raise ValueError('Original activation source changed')

    def ensure_secret(self,desired):
        self.guard();name=desired['metadata']['name']
        if self.original.get('privateSecrets',{}).get(name)!=desired:raise ValueError('Activation cannot create or change a credential')
        current=self.config.read('secret',name)
        if current is None:raise ValueError('Original activation credential unavailable')
        return current

    def preflight(self):
        self.guard();source=self.original['target'];values=source['values'];lock=source['images']
        installed=json.loads(values['build']['configJSON'])
        bundle={'version':'ai-sdlc.build-desired/v1','revision':lock['image']['tag'],'controllerDigest':values['build']['digest'],
            **{k:installed[k] for k in ('profile','slots','issueMaxAgeSeconds')}}
        build_runtime_install.preflight(self.config.base_run,self.config.kubectl,self.config.provider,self.config.cluster,lock,bundle)
        trust=json.loads(values['provenance']['trustJSON'])
        with tempfile.NamedTemporaryFile(mode='w',suffix='.json') as f:
            json.dump(trust,f);f.flush()
            result=provenance_runtime_install.configure(self.config.base_run,self.config.kubectl,self.config.provider,self.state,lock,bundle,Path(f.name),values['verifier']['digest'],self)
        if result!=values['provenance']:raise ValueError('Original provenance installation changed')
        approved={'target':json.loads(values['release']['targetJSON']),'images':{'deployerDigest':values['release']['digest']}}
        result=release_runtime_install.configure(self.config.base_run,self.config.kubectl,self.config.provider,lock,approved,trust,self)
        if result!=values['release']:raise ValueError('Original release target changed')
        self.guard()

    def save(self):platform.write_record(self.path,self.record)

    def begin(self):
        if self.record is not None:return
        self.observe('prepared',empty=True);self.preflight()
        source=self.original['target'];lock=source['images']
        def compile_build(value):
            return build_runtime_install.component_call(self.config.provider,lock['image']['repository']+'@'+lock['migrator']['digest'],'compile-build-installation',value)
        targets={}
        for phase in PHASES[1:]:
            values=phase_values(source['values'],phase,compile_build)
            targets[phase]={'images':lock,'values':values,'documents':self.config.render(values),'externalRuntime':source['externalRuntime']}
            observer=bootstrap.Preparation(None,[],self.config.provider,self.config.cluster,self.state,lock)
            observer.use_activation_render(targets[phase]['documents'],phase)
            if set(prepared_resources.documents(targets[phase]['documents']))!=set(source['resourceUIDs']):raise ValueError('Activation cannot add or remove runtime resources')
        # After arming, the only rendered change is the intake replica count.
        before=copy.deepcopy(targets['armed']['documents']);after=copy.deepcopy(targets['active']['documents'])
        for doc in before:
            if doc.get('kind')=='Deployment' and doc['metadata']['name']=='ai-sdlc-intake':doc['spec']['replicas']=1
        if before!=after:raise ValueError('Final activation must change only intake replicas')
        self.observe('prepared',empty=True)
        now=time.time()
        self.record={'version':VERSION,'preparedSHA256':self.original_hash,'chartSHA256':chart_digest(),'createdAt':now,'expiresAt':activation_expiry(source['values'],now),
            'completed':'prepared','transition':None,'targets':targets}
        self.save()

    def resolve(self):
        transition=self.record.get('transition')
        if transition is None:return self.observe(self.record['completed'])
        try:self.observe(transition['to'])
        except ValueError:
            self.observe(transition['from'])
            return None
        self.record['completed']=transition['to'];self.record['transition']=None;self.save()
        return True

    def apply(self,phase):
        if time.time()>=self.record['expiresAt']:raise ValueError('Original activation window expired; preserve custody')
        previous=PHASES[PHASES.index(phase)-1]
        if self.record['completed']!=previous:raise ValueError('Initial activation order changed')
        self.observe(previous,empty=True);self.preflight()
        transition={'from':previous,'to':phase}
        if self.record.get('transition') not in (None,transition):raise ValueError('Original activation transition changed')
        self.record['transition']=transition;self.save()
        # Reobserve after durable intent and immediately before the Helm effect.
        self.observe(previous,empty=True)
        if chart_digest()!=self.record['chartSHA256'] or self.config.render(self.describe(phase)['values'])!=self.describe(phase)['documents']:
            raise ValueError('Original reviewed activation chart changed')
        remaining=self.record['expiresAt']-time.time()
        if remaining<=0:raise ValueError('Original activation window expired before Helm')
        with tempfile.NamedTemporaryFile(mode='w',suffix='.json') as f:
            json.dump(self.describe(phase)['values'],f);f.flush()
            self.config.base_run([self.binaries['helm'],'upgrade','ai-sdlc',str(install.CHART),'--kubeconfig',str(self.state/'kubeconfig'),
                '--kube-context','kind-'+self.config.cluster,'-n',platform.NAMESPACE,'-f',f.name,'--timeout',str(max(1,min(300,int(remaining))))+'s'],capture_output=True,timeout=min(330,remaining))
        deadline=min(time.monotonic()+300,time.monotonic()+max(0,self.record['expiresAt']-time.time()))
        while True:
            try:self.observe(phase);break
            except ValueError:
                if time.monotonic()>=deadline:raise ValueError('Activation target has not converged; preserve original intent') from None
                time.sleep(2)
        self.record['completed']=phase;self.record['transition']=None;self.save()

    def publish_report(self):
        """Compatibility snapshot only; original custody remains authoritative."""
        if self.record.get('completed')!='active' or self.record.get('transition'):
            raise ValueError('Only completely active runtime can publish readiness')
        observed=self.observe('active') # current Ready replicas; never require empty workflow state
        original=self.original['target'];description=self.describe('active')
        if (description['images']!=original['images'] or any(observed.get(key)!=original['observed'].get(key)
                for key in ('clusterUID','namespaceUID'))):
            raise ValueError('Original installation identity changed before readiness report')
        values=original['values'];inner=json.loads(values['authority']['policyJSON'])
        build=json.loads(values['build']['configJSON'])
        repository=original['images']['image']['repository']
        prior=files.bounded_json(self.state/'installation.json',2*1024*1024)
        if (prior.get('cluster')!=self.config.cluster or prior.get('platform')!='konflux'
                or prior.get('repository')!=inner['repository'] or prior.get('environment')!=inner['environment']
                or prior.get('images')!=description['images'] or prior.get('namespace') not in (None,platform.NAMESPACE)
                or prior.get('configuration') not in ('prepared-paused','active')
                or prior.get('configuration')=='active' and prior.get('preparedSHA256')!=self.original_hash):
            raise ValueError('Original installation report scope changed')
        report={'cluster':self.config.cluster,'provider':self.config.provider,'platform':'konflux','mode':'verification',
            'namespace':platform.NAMESPACE,'clusterUID':observed['clusterUID'],'namespaceUID':observed['namespaceUID'],
            'configuration':'active','configurationReady':True,'installationReady':True,
            'runtimeReadinessValidated':True,'runtimePaused':False,'innerFrozen':False,'outerFrozen':False,
            'deliveryEnabled':True,'issueIntakeEnabled':True,'repository':inner['repository'],'environment':inner['environment'],
            'images':original['images'],'deployerImage':repository+'@'+values['release']['digest'],
            'buildControllerImage':repository+'@'+values['build']['digest'],'publisherImage':build['profile']['publisherImage'],
            'preparedSHA256':self.original_hash,'activationSHA256':bootstrap.digest(self.record),
            'observedAt':int(time.time()*1000),'workflowEvidence':False,'autonomousEndToEndValidated':False}
        platform.write_record(self.state/'installation.json',report)

    def activate(self,read_only=False):
        if read_only:
            if self.record is None or self.record.get('transition'):raise ValueError('Status requires a complete recorded activation phase')
            self.observe(self.record['completed'])
        else:
            self.begin();self.resolve()
            for phase in PHASES[PHASES.index(self.record['completed'])+1:]:self.apply(phase)
            self.publish_report()
        return {'version':VERSION,'phase':self.record['completed'],'issueIntakeEnabled':self.record['completed']=='active',
            'workflowEvidence':False,'autonomousEndToEndValidated':False}


def main(argv):
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command',choices=('activate','status'));parser.add_argument('--cluster',required=True)
    parser.add_argument('--provider',choices=('podman','docker','auto'),default='auto');args=parser.parse_args(argv)
    import os,re
    if not re.fullmatch('[a-z][a-z0-9-]{0,35}',args.cluster):raise ValueError('Selected local cluster required')
    state=Path(os.environ.get('XDG_STATE_HOME',Path.home()/'.local/state'))/'ai-sdlc'/args.cluster
    release_target_install.private_directory(state)
    with ExitStack() as stack:
        platform.lock_state(state,stack);provider=platform.select_provider(args.provider,state,args.cluster)
        binaries=install.tools(Path(os.environ.get('XDG_CACHE_HOME',Path.home()/'.cache'))/'ai-sdlc/tools')
        kubectl=[binaries['kubectl'],'--kubeconfig',str(state/'kubeconfig'),'--context','kind-'+args.cluster,'--request-timeout=10s']
        config=prepared_configuration.Configuration(install.run,kubectl,binaries,provider,args.cluster,state,{})
        print(json.dumps(Activation(config).activate(args.command=='status'),indent=2));return 0
